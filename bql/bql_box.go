package bql

import (
	"github.com/Sirupsen/logrus"
	"math/rand"
	"pfi/sensorbee/sensorbee/bql/execution"
	"pfi/sensorbee/sensorbee/bql/parser"
	"pfi/sensorbee/sensorbee/bql/udf"
	"pfi/sensorbee/sensorbee/core"
	"sync"
	"time"
)

type bqlBox struct {
	// stmt is the BQL statement executed by this box
	stmt *parser.SelectStmt
	// reg holds functions that can be used in this box
	reg udf.FunctionRegistry
	// plan is the execution plan for the SELECT statement in there
	execPlan execution.PhysicalPlan
	// mutex protects access to shared state
	mutex sync.Mutex
	// timeEmitterMutex protects access to those resources
	// accessed by the time-based emitter
	timeEmitterMutex sync.Mutex
	// emitterLimit holds a positive value if this box should
	// stop emitting items after a certain number of items
	emitterLimit int64
	// emitterSampling holds a positive value if this box should only
	// emit a certain subset of items (defined by emitterSamplingType)
	emitterSampling int64
	// emitterSamplingType holds a value different from
	// parser.UnspecifiedSamplingType if output sampling is active
	emitterSamplingType parser.EmitterSamplingType
	// genCount holds the number of items generated so far
	// (i.e. computed by the underlying execution plan). this is only
	// used if the count-based sampling is active.
	genCount int64
	// emitCount holds the number of items emitted so far
	emitCount int64
	// lastTuple points to the last tuple that was generated by
	// the underlying plan.
	lastTuple *core.Tuple
	// lastWriter points to the last writer that was passed to
	// `Process()`
	lastWriter core.Writer
	// stopped is an additional flag to signal the time-based emitter
	// that it should stop emitting items.
	stopped bool
	// removeMe is a function to remove this bqlBox from its
	// topology. A nil check must be done before calling.
	removeMe func()
}

func NewBQLBox(stmt *parser.SelectStmt, reg udf.FunctionRegistry) *bqlBox {
	return &bqlBox{stmt: stmt, reg: reg}
}

func (b *bqlBox) Init(ctx *core.Context) error {
	// create the execution plan
	analyzedPlan, err := execution.Analyze(*b.stmt, b.reg)
	if err != nil {
		return err
	}
	b.emitterLimit = analyzedPlan.EmitterLimit
	b.emitterSampling = analyzedPlan.EmitterSampling
	b.emitterSamplingType = analyzedPlan.EmitterSamplingType
	optimizedPlan, err := analyzedPlan.LogicalOptimize()
	if err != nil {
		return err
	}
	b.execPlan, err = optimizedPlan.MakePhysicalPlan(b.reg)
	if err != nil {
		return err
	}
	if b.emitterSamplingType == parser.TimeBasedSampling {
		go b.timeEmitter(ctx)
	}
	return nil
}

func (b *bqlBox) Process(ctx *core.Context, t *core.Tuple, s core.Writer) error {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// deal with statements that have an emitter limit. in particular,
	// if we are already over the limit, exit here
	if b.emitterLimit >= 0 && b.emitCount >= b.emitterLimit {
		return nil
	}

	// feed tuple into plan
	resultData, err := b.execPlan.Process(t)
	if err != nil {
		return err
	}

	// emit result data as tuples
	for _, data := range resultData {
		tup := &core.Tuple{
			Data:          data,
			Timestamp:     t.Timestamp,
			ProcTimestamp: t.ProcTimestamp,
			BatchID:       t.BatchID,
		}
		if len(t.Trace) != 0 {
			tup.Trace = make([]core.TraceEvent, len(t.Trace))
			copy(tup.Trace, t.Trace)
		}

		// decide if we should emit a tuple for this item
		shouldWriteTuple := true
		if b.emitterSamplingType == parser.CountBasedSampling {
			shouldWriteTuple = b.genCount%b.emitterSampling == 0
			// with 1,000,000 items per second, the counter below will
			// overflow after running for 292,471 years. probably ok.
			b.genCount += 1
		} else if b.emitterSamplingType == parser.RandomizedSampling {
			shouldWriteTuple = rand.Int63n(100) < b.emitterSampling
		} else if b.emitterSamplingType == parser.TimeBasedSampling {
			// we will never emit something from this function
			// when the time-based emitter is used
			b.timeEmitterMutex.Lock()
			b.lastTuple = tup
			b.lastWriter = s
			b.timeEmitterMutex.Unlock()
			continue
		}

		// write the tuple to the connected box
		if shouldWriteTuple {
			if err := s.Write(ctx, tup); err != nil {
				return err
			}
			b.emitCount += 1
		}
		// stop emitting if we have hit the limit
		if b.emitterLimit >= 0 && b.emitCount >= b.emitterLimit {
			break
		}
	}

	// remove this box if we are over the limit
	if b.emitterLimit >= 0 && b.emitCount >= b.emitterLimit {
		// avoid conflict with the timeEmitter (which will also perform
		// the same operation under some conditions)
		b.timeEmitterMutex.Lock()
		if b.removeMe != nil {
			b.removeMe()
			// don't call twice
			b.removeMe = nil
		}
		b.timeEmitterMutex.Unlock()
	}

	return nil
}

func (b *bqlBox) timeEmitter(ctx *core.Context) {
	// invariant: b.emitterSamplingType == TimeBasedSampling

	// generate a ticker that will tick every time we need to emit a tuple
	ticker := time.NewTicker(time.Duration(b.emitterSampling) * time.Millisecond)
	for _ = range ticker.C {
		// we need to lock here because we access the `stopped` flag, the
		// `lastTuple` and `lastWriter` pointer, as well as`emitCount`
		b.timeEmitterMutex.Lock()
		// b.stopped is set to true by either
		// - the Terminate function (in that case we may in no case
		//   write any further tuples to any writer)
		// - this function itself (if there is a LIMIT present that we hit)
		if b.stopped {
			b.timeEmitterMutex.Unlock()
			ticker.Stop()
			// eat the rest of the ticks
			continue
		}

		if b.lastTuple != nil && b.lastWriter != nil {
			if err := b.lastWriter.Write(ctx, b.lastTuple); err != nil {
				if ctx != nil {
					ctx.ErrLog(err).WithFields(logrus.Fields{
						"node_type": "box",
						"node_sink": b.lastWriter,
					}).Error("Cannot write tuple")
				}
			}
			// we do not want to emit the same tuple twice, so
			// we set it to null
			b.lastTuple = nil
			// increase the counter and check if we have reached the limit
			b.emitCount += 1
			if b.emitterLimit >= 0 && b.emitCount >= b.emitterLimit {
				b.stopped = true
				// if this function sets the b.stopped flag itself, it must
				// also remove the box from the topology. (if the b.stopped
				// flag is set by Terminate, we must not call removeMe,
				// therefore we cannot move this behind the loop.)
				if b.removeMe != nil {
					b.removeMe()
					// don't call twice
					b.removeMe = nil
				}
			}
		}
		// unlock the mutex while waiting
		b.timeEmitterMutex.Unlock()
	}
}

func (b *bqlBox) Terminate(ctx *core.Context) error {
	// signal to the time-based emitter that it should stop
	b.timeEmitterMutex.Lock()
	b.stopped = true
	b.timeEmitterMutex.Unlock()
	return nil
}
