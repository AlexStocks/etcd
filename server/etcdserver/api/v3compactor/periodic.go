// Copyright 2017 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v3compactor

import (
	"context"
	"fmt"
	"sync"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/mvcc"

	"github.com/jonboulle/clockwork"
	"go.uber.org/zap"
)

// PeriodicOption set periodic option
type PeriodicOption func(options *PeriodicOptions)

// PeriodicOptions defines Periodic config
type PeriodicOptions struct {
	specialFlag        bool
	specialStartHour   int
	specialStartMinute int
	specialEndHour     int
	specialEndMinute   int
	specialPeriod      time.Duration
}

func validatePeriodicOptions(opts PeriodicOptions) bool {
	if opts.specialStartHour < 0 || 23 < opts.specialStartHour {
		return false
	}
	if opts.specialStartMinute < 0 || 59 < opts.specialEndHour {
		return false
	}
	if opts.specialEndHour < 0 || 23 < opts.specialEndHour {
		return false
	}
	if opts.specialEndMinute < 0 || 59 < opts.specialEndHour {
		return false
	}

	if opts.specialEndHour < opts.specialStartHour {
		return false
	} else if opts.specialEndHour == opts.specialStartHour {
		if opts.specialEndMinute <= opts.specialStartMinute {
			return false
		}
	}

	return true
}

// WithSpecialStartHour set the special compaction start hour
func WithSpecialStartHour(startHour int) PeriodicOption {
	return func(o *PeriodicOptions) {
		o.specialStartHour = startHour
	}
}

// WithSpecialStartMinute set the special compaction start minute
func WithSpecialStartMinute(startMinute int) PeriodicOption {
	return func(o *PeriodicOptions) {
		o.specialStartMinute = startMinute
	}
}

// WithSpecialEndHour set the special compaction end hour
func WithSpecialEndHour(startHour int) PeriodicOption {
	return func(o *PeriodicOptions) {
		o.specialEndHour = startHour
	}
}

// WithSpecialEndMinute set the special compaction end minute
func WithSpecialEndMinute(startMinute int) PeriodicOption {
	return func(o *PeriodicOptions) {
		o.specialEndMinute = startMinute
	}
}

// WithSpecialPeriod sets the compaction interval in special compaction time span
func WithSpecialPeriod(specialPeriod time.Duration) PeriodicOption {
	return func(o *PeriodicOptions) {
		o.specialPeriod = specialPeriod
	}
}

// Periodic compacts the log by purging revisions older than
// the configured retention time.
type Periodic struct {
	lg     *zap.Logger
	clock  clockwork.Clock
	period time.Duration
	opts   PeriodicOptions

	rg RevGetter
	c  Compactable

	revs   []int64
	ctx    context.Context
	cancel context.CancelFunc

	// mu protects paused
	mu     sync.RWMutex
	paused bool
}

// newPeriodic creates a new instance of Periodic compactor that purges
// the log older than h Duration.
func newPeriodic(
	lg *zap.Logger,
	clock clockwork.Clock,
	h time.Duration,
	rg RevGetter,
	c Compactable,
	options ...PeriodicOption,
) (*Periodic, error) {
	pc := &Periodic{
		lg:     lg,
		clock:  clock,
		period: h,
		rg:     rg,
		c:      c,
		revs:   make([]int64, 0),
	}

	for _, opt := range options {
		opt(&(pc.opts))
	}
	pc.opts.specialFlag = validatePeriodicOptions(pc.opts)
	if pc.opts.specialFlag && pc.opts.specialPeriod == 0 {
		return nil, fmt.Errorf("illegal compact params: specialPeriod == 0")
	}
	if !pc.opts.specialFlag && pc.opts.specialPeriod > 0 {
		return nil, fmt.Errorf("illegal compact time span params")
	}

	pc.ctx, pc.cancel = context.WithCancel(context.Background())
	return pc, nil
}

/*
Compaction period 1-hour:
  1. compute compaction period, which is 1-hour
  2. record revisions for every 1/10 of 1-hour (6-minute)
  3. keep recording revisions with no compaction for first 1-hour
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 1-hour (6-minute)

Compaction period 24-hour:
  1. compute compaction period, which is 1-hour
  2. record revisions for every 1/10 of 1-hour (6-minute)
  3. keep recording revisions with no compaction for first 24-hour
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 1-hour (6-minute)

Compaction period 59-min:
  1. compute compaction period, which is 59-min
  2. record revisions for every 1/10 of 59-min (5.9-min)
  3. keep recording revisions with no compaction for first 59-min
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 59-min (5.9-min)

Compaction period 5-sec:
  1. compute compaction period, which is 5-sec
  2. record revisions for every 1/10 of 5-sec (0.5-sec)
  3. keep recording revisions with no compaction for first 5-sec
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 5-sec (0.5-sec)
*/

// Run runs periodic compactor.
func (pc *Periodic) Run() {
	retryInterval := pc.getRetryInterval()
	retentions := pc.getRetentions()

	go func() {
		lastSuccess := pc.clock.Now()
		baseInterval := pc.period
		for {
			pc.revs = append(pc.revs, pc.rg.Rev())
			if len(pc.revs) > retentions {
				pc.revs = pc.revs[1:] // pc.revs[0] is always the rev at pc.period ago
			}

			select {
			case <-pc.ctx.Done():
				return
			case <-pc.clock.After(retryInterval):
				pc.mu.Lock()
				p := pc.paused
				pc.mu.Unlock()
				if p {
					continue
				}
			}

			if pc.clock.Now().Sub(lastSuccess) < baseInterval {
				continue
			}

			// wait up to initial given period
			baseInterval = pc.getCompactInterval()
			rev := pc.revs[0]

			pc.lg.Info(
				"starting auto periodic compaction",
				zap.Int64("revision", rev),
				zap.Duration("compact-period", pc.period),
			)
			startTime := pc.clock.Now()
			_, err := pc.c.Compact(pc.ctx, &pb.CompactionRequest{Revision: rev})
			if err == nil || err == mvcc.ErrCompacted {
				pc.lg.Info(
					"completed auto periodic compaction",
					zap.Int64("revision", rev),
					zap.Duration("compact-period", pc.period),
					zap.Duration("took", pc.clock.Now().Sub(startTime)),
				)
				lastSuccess = pc.clock.Now()
			} else {
				pc.lg.Warn(
					"failed auto periodic compaction",
					zap.Int64("revision", rev),
					zap.Duration("compact-period", pc.period),
					zap.Duration("retry-interval", retryInterval),
					zap.Error(err),
				)
			}
		}
	}()
}

// if given compaction period x is <1-hour, compact every x duration.
// (e.g. --auto-compaction-mode 'periodic' --auto-compaction-retention='10m', then compact every 10-minute)
// if given compaction period x is >1-hour, compact every hour.
// (e.g. --auto-compaction-mode 'periodic' --auto-compaction-retention='2h', then compact every 1-hour)
func (pc *Periodic) getCompactInterval() time.Duration {
	itv := pc.period
	if pc.opts.specialFlag {
		now := time.Now()
		if now.Hour() > pc.opts.specialStartHour || (now.Hour() == pc.opts.specialStartHour && now.Minute() >= pc.opts.specialStartMinute) {
			if now.Hour() < pc.opts.specialEndHour || (now.Hour() == pc.opts.specialEndHour && now.Minute() <= pc.opts.specialEndMinute) {
				itv = pc.opts.specialPeriod
			}
		}
	}

	if itv > time.Hour {
		itv = time.Hour
	}
	return itv
}

func (pc *Periodic) getRetentions() int {
	return int(pc.getCompactInterval()/pc.getRetryInterval()) + 1
}

const retryDivisor = 10

func (pc *Periodic) getRetryInterval() time.Duration {
	itv := pc.getCompactInterval()
	if itv > time.Hour {
		itv = time.Hour
	}
	return itv / retryDivisor
}

// Stop stops periodic compactor.
func (pc *Periodic) Stop() {
	pc.cancel()
}

// Pause pauses periodic compactor.
func (pc *Periodic) Pause() {
	pc.mu.Lock()
	pc.paused = true
	pc.mu.Unlock()
}

// Resume resumes periodic compactor.
func (pc *Periodic) Resume() {
	pc.mu.Lock()
	pc.paused = false
	pc.mu.Unlock()
}
