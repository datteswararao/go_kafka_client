/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package go_kafka_client

import (
	"time"
	"math"
	"fmt"
	"sync"
)

type WorkerManager struct {
	Config *ConsumerConfig
	Strategy         WorkerStrategy
	FailureHook FailedCallback
	FailedAttemptHook FailedAttemptCallback
	Workers          []*Worker
	AvailableWorkers chan *Worker
	CurrentBatch map[TaskId]*Task
	InputChannel     chan [] *Message
	OutputChannel    chan map[TopicAndPartition]int64
	LargestOffsets map[TopicAndPartition]int64
	FailCounter *FailureCounter
	batchProcessed chan bool
	stopLock sync.Mutex
	stopped bool
}

func (wm *WorkerManager) String() string {
	return fmt.Sprintf("%s-workerManager", wm.Config.ConsumerId)
}

func (wm *WorkerManager) Start() {
	go wm.processBatch()
	for {
		if wm.stopped { return }
		batch := <-wm.InputChannel
		for _, message := range batch {
			wm.CurrentBatch[TaskId{ TopicAndPartition{ message.Topic, message.Partition }, message.Offset }] = &Task{
				Msg: message,
			}
		}

		for _, task := range wm.CurrentBatch {
			worker := <-wm.AvailableWorkers
			worker.Start(task, wm.Strategy)
		}

		<-wm.batchProcessed

		wm.OutputChannel <- wm.LargestOffsets
		wm.LargestOffsets = make(map[TopicAndPartition]int64)
	}
}

func (wm *WorkerManager) Stop() chan bool {
	finished := make(chan bool)
	go func() {
		InLock(&wm.stopLock, func() {
				wm.stopped = true
				for _, worker := range wm.Workers {
					result := worker.Close()
					if result != nil {
						wm.taskIsDone(result)
					}
				}
				wm.CurrentBatch = make(map[TaskId]*Task)
				wm.batchProcessed <- true
				finished <- true
			})
	}()

	return finished
}

func (wm *WorkerManager) IsBatchProcessed() bool {
	return len(wm.CurrentBatch) == 0
}

func (wm *WorkerManager) processBatch() {
	outputChannels := make([]*chan WorkerResult, wm.Config.NumWorkers)
	for i, worker := range wm.Workers {
		outputChannels[i] = &worker.OutputChannel
	}

	resultsChannel := make(chan WorkerResult)
	for {
		InLock(&wm.stopLock, func() {
				if wm.stopped == true {
					return
				}
				stopRedirecting := RedirectChannelsTo(outputChannels, resultsChannel)
				result := <- resultsChannel
				go func() {
					stopRedirecting <- true
				}()

				task := wm.CurrentBatch[result.Id()]
				if result.Success() {
					wm.taskIsDone(result)
				} else {
					if _, ok := result.(*TimedOutResult); ok {
						task.Callee.OutputChannel = make(chan WorkerResult)
					}

					Warnf(wm, "Worker task %s has failed", result.Id())
					task.Retries++
					if task.Retries >= wm.Config.MaxWorkerRetries {
						Errorf(wm, "Worker task %s has failed after %d retries", result.Id(), wm.Config.MaxWorkerRetries)

						var decision FailedDecision
						if wm.FailCounter.Failed() {
							decision = wm.FailureHook(wm)
						} else {
							decision = wm.FailedAttemptHook(task, result)
						}
						switch decision {
						case CommitOffsetAndContinue: {
							wm.taskIsDone(result)
						}
						case DoNotCommitOffsetAndContinue: {
							wm.AvailableWorkers <- wm.CurrentBatch[result.Id()].Callee
							delete(wm.CurrentBatch, result.Id())
						}
						case CommitOffsetAndStop: {
							wm.taskIsDone(result)
							wm.stopBatch()
						}
						case DoNotCommitOffsetAndStop: {
							wm.stopBatch()
						}
						}
					} else {
						Warnf(wm, "Retrying worker task %s %dth time", result.Id(), task.Retries)
						time.Sleep(wm.Config.WorkerBackoff)
						go task.Callee.Start(task, wm.Strategy)
					}
				}

				if wm.IsBatchProcessed() {
					wm.batchProcessed <- true
				}

		})
	}
}

func (wm *WorkerManager) stopBatch() {
	wm.CurrentBatch = make(map[TaskId]*Task)
	for _, worker := range wm.Workers {
		worker.OutputChannel = make(chan WorkerResult)
	}
}

func (wm *WorkerManager) taskIsDone(result WorkerResult) {
	key := result.Id().TopicPartition
	wm.LargestOffsets[key] = int64(math.Max(float64(wm.LargestOffsets[key]), float64(result.Id().Offset)))
	wm.AvailableWorkers <- wm.CurrentBatch[result.Id()].Callee
	delete(wm.CurrentBatch, result.Id())
}

func NewWorkerManager(config *ConsumerConfig, batchOutputChannel chan map[TopicAndPartition]int64) *WorkerManager {
	workers := make([]*Worker, config.NumWorkers)
	availableWorkers := make(chan *Worker, config.NumWorkers)
	for i := 0; i < config.NumWorkers; i++ {
		workers[i] = &Worker{
			OutputChannel: make(chan WorkerResult),
			CloseTimeout: config.WorkerCloseTimeout,
			TaskTimeout: config.WorkerTaskTimeout,
		}
		availableWorkers <- workers[i]
	}

	return &WorkerManager {
		Config: config,
		Strategy: config.Strategy,
		FailureHook: config.WorkerFailureCallback,
		FailedAttemptHook: config.WorkerFailedAttemptCallback,
		AvailableWorkers: availableWorkers,
		Workers: workers,
		InputChannel: make(chan [] *Message),
		OutputChannel: batchOutputChannel,
		CurrentBatch: make(map[TaskId]*Task),
		LargestOffsets: make(map[TopicAndPartition]int64),
		FailCounter: NewFailureCounter(config.WorkerRetryThreshold, config.WorkerConsideredFailedTimeWindow),
		batchProcessed: make(chan bool),
	}
}

type Worker struct {
	OutputChannel chan WorkerResult
	CloseTimeout time.Duration
	TaskTimeout time.Duration
	Closed bool
}

func (w *Worker) Start(task *Task, strategy WorkerStrategy) {
	task.Callee = w
	go func() {
		resultChannel := make(chan WorkerResult)
		go func() {resultChannel <- strategy(w, task.Msg, task.Id())}()
		select {
		case result := <-resultChannel: {
			w.OutputChannel <- result
		}
		case <-time.After(w.TaskTimeout): {
			w.OutputChannel <- &TimedOutResult{task.Id()}
		}
		}
	}()
}

func (w *Worker) Close() WorkerResult {
	if !w.Closed {
		w.Closed = true
		select {
		case result := <- w.OutputChannel: {
			return result
		}
		case <- time.After(w.CloseTimeout): {
			Warnf(w, "Worker failed to close within %f seconds", w.CloseTimeout.Seconds())
			return nil
		}
		}
	}

	return nil
}

type WorkerStrategy func(*Worker, *Message, TaskId) WorkerResult

type FailedCallback func(*WorkerManager) FailedDecision

type FailedAttemptCallback func(*Task, WorkerResult) FailedDecision

type FailureCounter struct {
	count int32
	failed bool
	countLock sync.Mutex
	failedThreshold int32
}

func NewFailureCounter(FailedThreshold int32, WorkerConsideredFailedTimeWindow time.Duration) *FailureCounter {
	counter := &FailureCounter {
		failedThreshold : FailedThreshold,
	}
	go func() {
		for {
			select {
			case <- time.After(WorkerConsideredFailedTimeWindow): {
				if counter.count >= FailedThreshold {
					counter.failed = true
					return
				} else {
					InLock(&counter.countLock, func() {
						counter.count = 0
					})
				}
			}
			}
		}
	}()
	return counter
}

func (f *FailureCounter) Failed() bool {
	InLock(&f.countLock, func() { f.count++ })
	return f.count >= f.failedThreshold || f.failed
}

type Task struct {
	Msg *Message
	Retries int
	Callee *Worker
}

func (t *Task) Id() TaskId {
	return TaskId{TopicAndPartition{t.Msg.Topic, t.Msg.Partition}, t.Msg.Offset}
}

type WorkerResult interface {
	Id() TaskId
	Success() bool
}

type SuccessfulResult struct {
	id TaskId
}

func NewSuccessfulResult(id TaskId) *SuccessfulResult {
	return &SuccessfulResult{id}
}

func (sr *SuccessfulResult) String() string {
	return fmt.Sprintf("{Success: %s}", sr.Id())
}

func (wr *SuccessfulResult) Id() TaskId {
	return wr.id
}

func (wr *SuccessfulResult) Success() bool {
	return true
}

type ProcessingFailedResult struct {
	id TaskId
}

func (sr *ProcessingFailedResult) String() string {
	return fmt.Sprintf("{Failed: %s}", sr.Id())
}

func (wr *ProcessingFailedResult) Id() TaskId {
	return wr.id
}

func (wr *ProcessingFailedResult) Success() bool {
	return false
}

type TimedOutResult struct {
	id TaskId
}

func (sr *TimedOutResult) String() string {
	return fmt.Sprintf("{Timed out: %s}", sr.Id())
}

func (wr *TimedOutResult) Id() TaskId {
	return wr.id
}

func (wr *TimedOutResult) Success() bool {
	return false
}

type TaskId struct {
	TopicPartition TopicAndPartition
	Offset         int64
}

func (tid TaskId) String() string {
	return fmt.Sprintf("%s, Offset: %d", &tid.TopicPartition, tid.Offset)
}

type FailedDecision int32

const (
	CommitOffsetAndContinue FailedDecision = iota
	DoNotCommitOffsetAndContinue
	CommitOffsetAndStop
	DoNotCommitOffsetAndStop
)
