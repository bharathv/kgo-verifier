package verifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rcrowley/go-metrics"
	"github.com/redpanda-data/kgo-verifier/pkg/util"
	worker "github.com/redpanda-data/kgo-verifier/pkg/worker"
	log "github.com/sirupsen/logrus"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/semaphore"
)

type TransactionalProducerConfig struct {
	workerCfg          worker.WorkerConfig
	name               string
	nPartitions        int32
	messageSize        int
	messageCount       int
	fakeTimestampMs    int64
	abortRate          float64
	msgsPerTransaction uint
}

func NewTransactionalProducerConfig(wc worker.WorkerConfig, name string, nPartitions int32,
	messageSize int, messageCount int, fakeTimestampMs int64, abortRate float64, msgsPerTransaction uint) TransactionalProducerConfig {
	return TransactionalProducerConfig{
		workerCfg:          wc,
		name:               name,
		nPartitions:        nPartitions,
		messageCount:       messageCount,
		messageSize:        messageSize,
		fakeTimestampMs:    fakeTimestampMs,
		abortRate:          abortRate,
		msgsPerTransaction: msgsPerTransaction,
	}
}

type TransactionalProducerWorker struct {
	config          TransactionalProducerConfig
	Status          TransactionalProducerWorkerStatus
	validOffsets    TopicOffsetRanges
	fakeTimestampMs int64
}

func NewTransactionalProducerWorker(cfg TransactionalProducerConfig) TransactionalProducerWorker {
	return TransactionalProducerWorker{
		config:          cfg,
		Status:          NewTransactionalProducerWorkerStatus(),
		validOffsets:    LoadTopicOffsetRanges(cfg.workerCfg.Topic, cfg.nPartitions),
		fakeTimestampMs: cfg.fakeTimestampMs,
	}
}

func (pw *TransactionalProducerWorker) newRecord(producerId int, sequence int64, aborted bool) *kgo.Record {
	var key bytes.Buffer

	if !aborted {
		fmt.Fprintf(&key, "%06d.%018d", producerId, sequence)
	} else {
		// This message ensures that `ValidatorStatus.ValidateRecord`
		// will report it as an invalid read if it's consumed. This is
		// since messages in aborted transactions should never be read.
		fmt.Fprintf(&key, "ABORTED MSG: %06d.%018d", producerId, sequence)
	}

	payload := make([]byte, pw.config.messageSize)

	var r *kgo.Record = kgo.KeySliceRecord(key.Bytes(), payload)

	if pw.fakeTimestampMs != -1 {
		r.Timestamp = time.Unix(0, pw.fakeTimestampMs*1000000)
		pw.fakeTimestampMs += 1
	}
	return r
}

type TransactionalProducerWorkerStatus struct {
	// How many messages did we try to transmit?
	Sent int64 `json:"sent"`

	// How many messages did we send successfully (were acked
	// by the server at the offset we expected)?
	Acked int64 `json:"acked"`

	// How many messages landed at an unexpected offset?
	// (indicates retries/resends)
	BadOffsets int64 `json:"bad_offsets"`

	// How many failures occured while trying to begin, abort,
	// or commit a transaction.
	FailedTransactions int64 `json:"failed_transactions"`

	// How many times did we restart the producer loop?
	Restarts int64 `json:"restarts"`

	// Ack latency: a private histogram for the data,
	// and a public summary for JSON output
	latency metrics.Histogram
	Latency worker.HistogramSummary `json:"latency"`

	Active bool `json:"latency"`

	lock sync.Mutex

	// For emitting checkpoints on time intervals
	lastCheckpoint time.Time
}

func NewTransactionalProducerWorkerStatus() TransactionalProducerWorkerStatus {
	return TransactionalProducerWorkerStatus{
		lastCheckpoint: time.Now(),
		latency:        metrics.NewHistogram(metrics.NewExpDecaySample(1024, 0.015)),
	}
}

func (self *TransactionalProducerWorkerStatus) OnAcked() {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.Acked += 1
}

func (self *TransactionalProducerWorkerStatus) OnBadOffset() {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.BadOffsets += 1
}

func (pw *TransactionalProducerWorker) produceCheckpoint() {
	err := pw.validOffsets.Store()
	util.Chk(err, "Error writing offset map: %v", err)

	data, err := json.Marshal(pw.Status)
	util.Chk(err, "Status serialization error")
	log.Infof("TransactionalProducer status: %s", data)
}

func (pw *TransactionalProducerWorker) Wait() error {
	pw.Status.Active = true
	defer func() { pw.Status.Active = false }()

	n := int64(pw.config.messageCount)

	for {
		n_produced, bad_offsets, err := pw.produceInner(n)
		if err != nil {
			return err
		}
		n = n - n_produced

		if len(bad_offsets) > 0 {
			log.Infof("Produce stopped early, %d still to do", n)
		}

		if n <= 0 {
			return nil
		} else {
			// Record that we took another run at produceInner
			pw.Status.Restarts += 1
		}
	}
}

func (pw *TransactionalProducerWorker) produceInner(n int64) (int64, []BadOffset, error) {
	opts := pw.config.workerCfg.MakeKgoOpts()
	randId := uuid.New()

	opts = append(opts, []kgo.Opt{
		kgo.ProducerBatchCompression(kgo.NoCompression()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.TransactionalID("p" + randId.String()),
		kgo.TransactionTimeout(2 * time.Minute),
	}...)
	client, err := kgo.NewClient(opts...)
	if err != nil {
		log.Errorf("Error creating Kafka client: %v", err)
		return 0, nil, err
	}

	currentOffsets := GetOffsets(client, pw.config.workerCfg.Topic, pw.config.nPartitions, -1)

	for i, o := range currentOffsets {
		log.Infof("Produce start offset %s/%d %d...", pw.config.workerCfg.Topic, i, o)
	}

	var wg sync.WaitGroup

	errored := false
	produced := int64(0)

	// Channel must be >= concurrency
	bad_offsets := make(chan BadOffset, 16384)
	concurrent := semaphore.NewWeighted(4096)

	log.Infof("Producing %d messages (%d bytes)", n, pw.config.messageSize)

	if err := client.BeginTransaction(); err != nil {
		log.Errorf("Couldn't start a transaction: %v", err)
		pw.Status.FailedTransactions += 1
		return 0, nil, err
	}

	// BeginTransaction will leave one control record in
	// every partition's log.
	for i, _ := range currentOffsets {
		currentOffsets[i] += 1
	}

	willAbort := pw.config.abortRate >= rand.Float64()

	for i := int64(0); i < n && len(bad_offsets) == 0; i = i + 1 {
		concurrent.Acquire(context.Background(), 1)
		produced += 1
		pw.Status.Sent += 1
		var p = rand.Int31n(pw.config.nPartitions)

		if i > 0 && i%int64(pw.config.msgsPerTransaction) == 0 {
			if err := client.Flush(context.Background()); err != nil {
				log.Errorf("Unable to flush: %v", err)
				errored = true
				pw.Status.FailedTransactions += 1
				break
			}
			if err := client.EndTransaction(context.Background(), kgo.TransactionEndTry(!willAbort)); err != nil {
				log.Errorf("unable to end transaction: %v", err)
				errored = true
				pw.Status.FailedTransactions += 1
				break
			}
			if err := client.BeginTransaction(); err != nil {
				log.Errorf("Couldn't start a transaction: %v", err)
				errored = true
				pw.Status.FailedTransactions += 1
				break
			}

			for i, _ := range currentOffsets {
				if !willAbort {
					// EndTransaction and BeginTransaction will each leave
					// one control record in each partition's log.
					currentOffsets[i] += 2
				} else {
					// In this case a transaction that was just aborted.
					// A abort doesn't leave a control record currently
					// So just account for the control record left by BeginTransaction
					// TODO: this behavior has changed on the tip of dev and aborts
					// now leave a control record
					currentOffsets[i] += 1
				}
			}

			// Decide if the newly started transaction will be aborted or not
			willAbort = pw.config.abortRate >= rand.Float64()
		}

		currentOffset := currentOffsets[p]
		currentOffsets[p] += 1

		r := pw.newRecord(0, currentOffset, willAbort)
		r.Partition = p
		wg.Add(1)

		sentAt := time.Now()
		handler := func(r *kgo.Record, err error) {
			concurrent.Release(1)
			util.Chk(err, "Produce failed: %v", err)

			if r.Offset != currentOffset {
				log.Warnf("Produced at unexpected offset %d (expected %d) on partition %d", r.Offset, currentOffset, r.Partition)
				pw.Status.OnBadOffset()
				bad_offsets <- BadOffset{r.Partition, r.Offset}
				errored = true
				log.Debugf("errored = %b", errored)
			} else {
				ackLatency := time.Now().Sub(sentAt)
				pw.Status.OnAcked()
				pw.Status.latency.Update(ackLatency.Microseconds())
				log.Debugf("Wrote partition %d at %d", r.Partition, r.Offset)

				pw.validOffsets.Insert(r.Partition, r.Offset)
			}
			wg.Done()
		}
		client.Produce(context.Background(), r, handler)

		// Not strictly necessary, but useful if a long running producer gets killed
		// before finishing

		if time.Since(pw.Status.lastCheckpoint) > 5*time.Second {
			pw.Status.lastCheckpoint = time.Now()
			pw.produceCheckpoint()
		}
	}

	if err := client.Flush(context.Background()); err != nil {
		log.Errorf("Unable to flush: %v", err)
		errored = true
		pw.Status.FailedTransactions += 1
	}
	if err := client.EndTransaction(context.Background(), kgo.TransactionEndTry(!willAbort)); err != nil {
		log.Errorf("unable to end transaction: %v", err)
		errored = true
		pw.Status.FailedTransactions += 1
	}

	log.Info("Waiting...")
	wg.Wait()
	log.Info("Waited.")
	wg.Wait()
	close(bad_offsets)

	pw.produceCheckpoint()

	if errored {
		log.Warnf("%d bad offsets", len(bad_offsets))
		var r []BadOffset
		for o := range bad_offsets {
			r = append(r, o)
		}
		successful_produced := produced - int64(len(r))
		return successful_produced, r, nil
	} else {
		wg.Wait()
		return produced, nil, nil
	}
}

func (pw *TransactionalProducerWorker) ResetStats() {
	pw.Status = NewTransactionalProducerWorkerStatus()
}

func (pw *TransactionalProducerWorker) GetStatus() interface{} {
	// Update public summary from private statustics
	pw.Status.Latency = worker.SummarizeHistogram(&pw.Status.latency)

	return &pw.Status
}
