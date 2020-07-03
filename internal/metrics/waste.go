// Copyright (c) 2019 Palantir Technologies. All rights reserved.
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

package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/palantir/k8s-spark-scheduler-lib/pkg/apis/scaler/v1alpha1"
	"github.com/palantir/k8s-spark-scheduler/internal"
	"github.com/palantir/k8s-spark-scheduler/internal/common/utils"
	"github.com/palantir/k8s-spark-scheduler/internal/crd"
	"github.com/palantir/pkg/metrics"
	"github.com/palantir/witchcraft-go-logging/wlog/svclog/svc1log"
	v1 "k8s.io/api/core/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientcache "k8s.io/client-go/tools/cache"
)

const (
	demandFulfilledAgeCleanUp = 6 * time.Hour
)

var (
	beforeDemandCreation = tagInfo{
		tag:              metrics.MustNewTag(schedulingWasteTypeTagName, "before-demand-creation"),
		slowLogThreshold: 1 * time.Minute,
	}
	afterDemandFulfilledNoFailures = tagInfo{
		tag:              metrics.MustNewTag(schedulingWasteTypeTagName, "after-demand-fulfilled-no-failures"),
		slowLogThreshold: 1 * time.Minute,
	}
	afterDemandFulfilledLastFailure = tagInfo{
		tag:              metrics.MustNewTag(schedulingWasteTypeTagName, "after-demand-fulfilled-last-failure"),
		slowLogThreshold: 1 * time.Minute,
	}
	totalTimeNoDemand = tagInfo{
		tag:              metrics.MustNewTag(schedulingWasteTypeTagName, "total-time-no-demand"),
		slowLogThreshold: 10 * time.Minute,
	}
)

func getTagInfoForFailureAfterDemandFulfilled(outcome string) tagInfo {
	return tagInfo{
		tag:              metrics.MustNewTag(schedulingWasteTypeTagName, "after-demand-fulfilled-failure-" + outcome),
		slowLogThreshold: 1 * time.Minute,
	}
}

type wasteMetricsReporter struct {
	ctx          				context.Context
	info               			schedulingMetricInfoByPod
	instanceGroupLabel 			string
	failedSchedulingAttempts 	chan podFailedSchedulingAttempt
	lock               			sync.Mutex
}

// StartSchedulingOverheadMetrics will start tracking demand creation an fulfillment times
// and report scheduling wasted time per pod
func StartSchedulingOverheadMetrics(
	ctx context.Context,
	podInformer coreinformers.PodInformer,
	demandInformer *crd.LazyDemandInformer,
	instanceGroupLabel string,
) {
	reporter := &wasteMetricsReporter{
		ctx:                ctx,
		info:               make(schedulingMetricInfoByPod),
		instanceGroupLabel: instanceGroupLabel,
		failedSchedulingAttempts: make(chan podFailedSchedulingAttempt, 100),
	}

	podInformer.Informer().AddEventHandler(
		clientcache.FilteringResourceEventHandler{
			FilterFunc: utils.IsSparkSchedulerPod,
			Handler: clientcache.ResourceEventHandlerFuncs{
				UpdateFunc: utils.OnPodScheduled(ctx, reporter.onPodScheduled),
				DeleteFunc: reporter.onPodDeleted,
			},
		},
	)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-demandInformer.Ready():
			informer, _ := demandInformer.Informer()
			informer.Informer().AddEventHandler(
				clientcache.FilteringResourceEventHandler{
					FilterFunc: utils.IsSparkSchedulerDemand,
					Handler: clientcache.ResourceEventHandlerFuncs{
						AddFunc:    reporter.onDemandCreated,
						UpdateFunc: utils.OnDemandFulfilled(ctx, reporter.onDemandFulfilled),
					},
				},
			)
		}
	}()

	go func() {
		t := time.NewTicker(demandFulfilledAgeCleanUp)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reporter.cleanupMetricCache()
			}
		}
	}()

	go func() {
		for {
			select {
			case <- ctx.Done():
				return
			case event := <- reporter.failedSchedulingAttempts:
				reporter.processFailedSchedulingAttemptEvent(event)
			}
		}
	}()
}

type failedSchedulingAttemptInfo struct {
	attemptTime time.Time
	attemptOutcome string
}

type schedulingMetricInfo struct {
	lastFailedSchedulingAttemptInfo failedSchedulingAttemptInfo
	demandFulfilledTime time.Time
	demandCreationTime  time.Time
}

type podKey struct {
	Namespace string
	Name      string
}

type podFailedSchedulingAttempt struct {
	podKey podKey
	failedSchedulingAttemptInfo failedSchedulingAttemptInfo
}

type schedulingMetricInfoByPod map[podKey]schedulingMetricInfo

func (r *wasteMetricsReporter) MarkFailedSchedulingAttempt(pod *v1.Pod, outcome string) {
	schedulingAttemptInfo := podFailedSchedulingAttempt{
		podKey: podKey{pod.Namespace, pod.Name},
		failedSchedulingAttemptInfo: failedSchedulingAttemptInfo{time.Now(), outcome},
	}
	select {
	case r.failedSchedulingAttempts <- schedulingAttemptInfo:
	default:
		svc1log.FromContext(r.ctx).Warn("Failed scheduling attempts channel full. Dropping event from waste scheduling reporter",
			svc1log.SafeParam("podNamespace", pod.Namespace),
			svc1log.SafeParam("Name", pod.Name),
			svc1log.SafeParam("outcome", outcome))
	}
}

func (r *wasteMetricsReporter) processFailedSchedulingAttemptEvent(event podFailedSchedulingAttempt) {
	r.lock.Lock()
	defer r.lock.Unlock()

	info, ok := r.info[event.podKey]
	if ok {
		r.info[event.podKey] = schedulingMetricInfo{
			lastFailedSchedulingAttemptInfo: event.failedSchedulingAttemptInfo,
			demandCreationTime: info.demandCreationTime,
			demandFulfilledTime: info.demandFulfilledTime,
		}
	} else {
		r.info[event.podKey] = schedulingMetricInfo{
			lastFailedSchedulingAttemptInfo: event.failedSchedulingAttemptInfo,
		}
	}
}

func (r *wasteMetricsReporter) onPodScheduled(pod *v1.Pod) {
	r.lock.Lock()
	defer r.lock.Unlock()

	info, ok := r.info[podKey{pod.Namespace, pod.Name}]
	if !ok || info.demandCreationTime.IsZero() {
		r.markAndSlowLog(pod, totalTimeNoDemand, time.Now().Sub(pod.CreationTimestamp.Time))
	} else {
		r.markAndSlowLog(pod, beforeDemandCreation, info.demandCreationTime.Sub(pod.CreationTimestamp.Time))

		if !info.demandFulfilledTime.IsZero() {
			if !info.lastFailedSchedulingAttemptInfo.attemptTime.IsZero() && info.lastFailedSchedulingAttemptInfo.attemptTime.After(info.demandFulfilledTime) {
				// the demand was fulfilled but there were still failures after that
				r.markAndSlowLog(pod, getTagInfoForFailureAfterDemandFulfilled(info.lastFailedSchedulingAttemptInfo.attemptOutcome),
					info.lastFailedSchedulingAttemptInfo.attemptTime.Sub(info.demandFulfilledTime))
				r.markAndSlowLog(pod, afterDemandFulfilledLastFailure, time.Now().Sub(info.lastFailedSchedulingAttemptInfo.attemptTime))
			} else {
				r.markAndSlowLog(pod, afterDemandFulfilledNoFailures, time.Now().Sub(info.demandFulfilledTime))
			}
		}
	}
}

func (r *wasteMetricsReporter) markAndSlowLog(pod *v1.Pod, tag tagInfo, duration time.Duration) {
	instanceGroup, _ := internal.FindInstanceGroupFromPodSpec(pod.Spec, r.instanceGroupLabel)
	if duration > tag.slowLogThreshold {
		svc1log.FromContext(r.ctx).Info("pod wait time is above threshold",
			svc1log.SafeParam("podNamespace", pod.Namespace),
			svc1log.SafeParam("podName", pod.Name),
			svc1log.SafeParam("instanceGroup", instanceGroup),
			svc1log.SafeParam("waitType", tag.tag.Value()),
			svc1log.SafeParam("duration", duration))
	}
	metrics.FromContext(r.ctx).Histogram(schedulingWaste, tag.tag).Update(duration.Nanoseconds())
	metrics.FromContext(r.ctx).Histogram(schedulingWastePerInstanceGroup, tag.tag, InstanceGroupTag(r.ctx, instanceGroup)).Update(duration.Nanoseconds())
}

func (r *wasteMetricsReporter) onDemandFulfilled(demand *v1alpha1.Demand) {
	r.lock.Lock()
	defer r.lock.Unlock()

	podKey := podKey{demand.Namespace, utils.PodName(demand)}
	info, ok := r.info[podKey]
	if ok {
		r.info[podKey] = schedulingMetricInfo{
			lastFailedSchedulingAttemptInfo: info.lastFailedSchedulingAttemptInfo,
			demandFulfilledTime: time.Now(),
			demandCreationTime:  demand.CreationTimestamp.Time,
		}
	} else {
		r.info[podKey] = schedulingMetricInfo{
			demandFulfilledTime: time.Now(),
			demandCreationTime:  demand.CreationTimestamp.Time,
		}
	}
}

func (r *wasteMetricsReporter) onDemandCreated(obj interface{}) {
	r.lock.Lock()
	defer r.lock.Unlock()
	demand, ok := obj.(*v1alpha1.Demand)
	if !ok {
		svc1log.FromContext(r.ctx).Error("failed to parse obj as demand")
		return
	}
	podKey := podKey{demand.Namespace, utils.PodName(demand)}
	info, ok := r.info[podKey]
	if ok {
		r.info[podKey] = schedulingMetricInfo{
			lastFailedSchedulingAttemptInfo: info.lastFailedSchedulingAttemptInfo,
			demandCreationTime:  demand.CreationTimestamp.Time,
		}
	} else {
		r.info[podKey] = schedulingMetricInfo{
			demandCreationTime:  demand.CreationTimestamp.Time,
		}
	}
}

func (r *wasteMetricsReporter) onPodDeleted(obj interface{}) {
	r.lock.Lock()
	defer r.lock.Unlock()

	pod, ok := utils.GetPodFromObjectOrTombstone(obj)
	if !ok {
		svc1log.FromContext(r.ctx).Warn("failed to parse object as pod, skipping")
		return
	}
	podKey := podKey{pod.Namespace, pod.Name}
	delete(r.info, podKey)
}

func (r *wasteMetricsReporter) cleanupMetricCache() {
	r.lock.Lock()
	defer r.lock.Unlock()
	for key, info := range r.info {
		if info.demandFulfilledTime.Add(demandFulfilledAgeCleanUp).Before(time.Now()) {
			svc1log.FromContext(r.ctx).Info(
				"deleting demand from scheduling waste reporter, pod was not scheduled for 6 hours",
				svc1log.SafeParam("podNamespace", key.Namespace),
				svc1log.SafeParam("podNamespace", key.Name),
			)
			delete(r.info, key)
		}
	}

}

type tagInfo struct {
	tag              metrics.Tag
	slowLogThreshold time.Duration
}
