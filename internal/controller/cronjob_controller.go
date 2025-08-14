/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cronjobv1 "github.com/Whitea029/cronjob-controller/api/v1"
	"github.com/robfig/cron"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock
}

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() } //nolint:staticcheck

// Clock knows how to get the current time.
// It can be used to fake out timing for testing.
type Clock interface {
	Now() time.Time
}

var (
	scheduledTimeAnnotation = "batch.whitea.com/scheduled-at"
)

// +kubebuilder:rbac:groups=batch.whitea.com,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch.whitea.com,resources=cronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch.whitea.com,resources=cronjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cronjob cronjobv1.CronJob
	if err := r.Client.Get(ctx, req.NamespacedName, &cronjob); err != nil {
		klog.Errorf("unable to fetch CronJob %s: %v", req.NamespacedName, err)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var childJobs batchv1.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		klog.Errorf("unable to list Jobs for CronJob %s: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	var activeJobs, successfulJobs, failedJobs []*batchv1.Job
	var mostRecentTime *time.Time

	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case batchv1.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		case batchv1.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		default:
			activeJobs = append(activeJobs, &childJobs.Items[i])
		}

		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			klog.Errorf("unable to parse scheduled time for Job %s: %v", job.Name, err)
			continue
		}
		if scheduledTimeForJob != nil {
			if mostRecentTime == nil || scheduledTimeForJob.After(*mostRecentTime) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	if mostRecentTime != nil {
		cronjob.Status.LastScheduledTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		cronjob.Status.LastScheduledTime = nil
	}

	cronjob.Status.Active = nil
	for _, activeJob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			klog.Errorf("unable to get reference for active Job %s: %v", activeJob.Name, err)
			continue
		}
		cronjob.Status.Active = append(cronjob.Status.Active, *jobRef)
	}

	klog.V(1).Infof("CronJob %s has %d active jobs, %d successful jobs, and %d failed jobs",
		req.NamespacedName, len(cronjob.Status.Active), len(successfulJobs), len(failedJobs))

	if err := r.Status().Update(ctx, &cronjob); err != nil {
		klog.Errorf("unable to update status for CronJob %s: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	if cronjob.Spec.FailedJobsHistoryLimit != nil {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*cronjob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Client.Delete(ctx, job); err != nil {
				klog.Errorf("unable to delete failed Job %s: %v", job.Name, err)
				return ctrl.Result{}, err
			}
			klog.Infof("Deleted failed Job %s for CronJob %s", job.Name, req.NamespacedName)
		}
	}

	if cronjob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i, job := range successfulJobs {
			if int32(i) >= int32(len(successfulJobs))-*cronjob.Spec.SuccessfulJobsHistoryLimit {
				break
			}
			if err := r.Client.Delete(ctx, job); err != nil {
				klog.Errorf("unable to delete successful Job %s: %v", job.Name, err)
				return ctrl.Result{}, err
			}
			klog.Infof("Deleted successful Job %s for CronJob %s", job.Name, req.NamespacedName)
		}
	}

	if cronjob.Spec.Suspend != nil && *cronjob.Spec.Suspend {
		klog.Infof("CronJob %s is suspended, skipping job creation", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	missedRun, nextRun, err := getNextScheduledTime(&cronjob, r.Now())
	if err != nil {
		klog.Errorf("unable to get next scheduled time for CronJob %s: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	klog.Infof("CronJob %s next run at %s, missed run at %s", req.NamespacedName, nextRun, missedRun)

	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())}

	if missedRun.IsZero() {
		klog.Infof("No missed runs for CronJob %s", req.NamespacedName)
		return scheduledResult, nil
	}

	klog.Infof("Missed run for CronJob %s at %s", req.NamespacedName, missedRun)

	tooLate := false
	if cronjob.Spec.StartingDeadlineSeconds != nil {
		tooLate = missedRun.Add(time.Duration(*cronjob.Spec.StartingDeadlineSeconds) * time.Second).Before(r.Now())
	}

	if tooLate {
		klog.Infof("Missed run for CronJob %s at %s is too late, skipping", req.NamespacedName, missedRun)
		return scheduledResult, nil
	}

	if cronjob.Spec.ConcurrencyPolicy == cronjobv1.ForbidConcurrent && len(activeJobs) > 0 {
		klog.Infof("Skipping missed run for CronJob %s at %s due to ForbidConcurrent policy", req.NamespacedName, missedRun)
		return scheduledResult, nil
	} else if cronjob.Spec.ConcurrencyPolicy == cronjobv1.ReplaceConcurrent && len(activeJobs) > 0 {
		for _, activeJob := range activeJobs {
			if err := r.Delete(ctx, activeJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				klog.Errorf("unable to delete active Job %s for CronJob %s: %v", activeJob.Name, req.NamespacedName, err)
				return ctrl.Result{}, err
			}
		}
	}

	job, err := constructJobForCronJob(&cronjob, missedRun, r.Scheme)
	if err != nil {
		klog.Errorf("unable to construct Job for CronJob %s: %v", req.NamespacedName, err)
		return scheduledResult, err
	}
	if err := r.Create(ctx, job); err != nil {
		klog.Infof("unable to create Job for CronJob %s: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	klog.Infof("Created Job %s for CronJob %s at %s", job.Name, req.NamespacedName, missedRun)

	return scheduledResult, nil
}

func constructJobForCronJob(cronJob *cronjobv1.CronJob, scheduledTime time.Time, schema *runtime.Scheme) (*batchv1.Job, error) {
	name := fmt.Sprintf("%s-%d", cronJob.Name, scheduledTime.Unix())
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
			Name:        name,
			Namespace:   cronJob.Namespace,
		},
		Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
	}
	for k, v := range cronJob.Spec.JobTemplate.Annotations {
		job.Annotations[k] = v
	}
	job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	for k, v := range cronJob.Spec.JobTemplate.Labels {
		job.Labels[k] = v
	}
	if err := ctrl.SetControllerReference(cronJob, job, schema); err != nil {
		return nil, err
	}
	return job, nil
}

func isJobFinished(job *batchv1.Job) (bool, batchv1.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

func getScheduledTimeForJob(job *batchv1.Job) (*time.Time, error) {
	timeRaw := job.Annotations[scheduledTimeAnnotation]
	if timeRaw == "" {
		return nil, nil
	}
	timeParsed, err := time.Parse(time.RFC3339, timeRaw)
	if err != nil {
		return nil, err
	}
	return &timeParsed, nil
}

func getNextScheduledTime(cronjob *cronjobv1.CronJob, now time.Time) (lastMissTime, nextTime time.Time, err error) {
	sched, err := cron.ParseStandard(cronjob.Spec.Schedule)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("unparseable schedule %q: %w", cronjob.Spec.Schedule, err)
	}
	var earliestTime time.Time
	if cronjob.Status.LastScheduledTime != nil {
		earliestTime = cronjob.Status.LastScheduledTime.Time
	} else {
		earliestTime = cronjob.CreationTimestamp.Time
	}

	if cronjob.Spec.StartingDeadlineSeconds != nil {
		schedulingDeadline := now.Add(-time.Second * time.Duration(*cronjob.Spec.StartingDeadlineSeconds))

		if schedulingDeadline.After(earliestTime) {
			earliestTime = schedulingDeadline
		}
	}

	if earliestTime.After(now) {
		return time.Time{}, sched.Next(now), nil
	}

	start := 0
	for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
		lastMissTime = t
		start++
		if start > 100 {
			return time.Time{}, time.Time{}, fmt.Errorf("schedule %q is too frequent, skipping", cronjob.Spec.Schedule)
		}
	}

	return lastMissTime, sched.Next(now), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &batchv1.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		job := rawObj.(*batchv1.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		if owner.APIVersion != apiGVStr || owner.Kind != "CronJob" {
			return nil
		}
		return []string{owner.Name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cronjobv1.CronJob{}).
		Named("cronjob").
		Complete(r)
}

var (
	jobOwnerKey = ".metadata.controller"
	apiGVStr    = cronjobv1.GroupVersion.String()
)
