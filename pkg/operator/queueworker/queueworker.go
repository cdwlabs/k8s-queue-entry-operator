package queueworker

import (
	"fmt"
	"github.com/golang/glog"
	"github.com/podnov/k8s-queue-entry-operator/pkg/operator/utils"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"time"
)

const jobQueueEntryKeyAnnotationKey = "queueentryoperator.evanzeimet.com/queue-entry-key"
const jobQueueScopeAnnotationKey = "queueentryoperator.evanzeimet.com/queue-scope"

func (w *QueueWorker) cleanUp() {
	jobConfig := w.queueResource.GetJobConfig()

	failedJobsHistoryLimit := jobConfig.FailedJobsHistoryLimit
	successfulJobsHistoryLimit := jobConfig.SuccessfulJobsHistoryLimit

	if failedJobsHistoryLimit != nil || successfulJobsHistoryLimit != nil {
		namespace := w.queueResource.GetObjectMeta().Namespace
		jobs, err := w.clientset.BatchV1().Jobs(namespace).List(metav1.ListOptions{})
		if err != nil {
			runtime.HandleError(fmt.Errorf("Error fetching Jobs: %v", err))
			return
		}

		ownedCompletedJobs, ownedFailedJobs := w.getOwnedFinishedJobs(jobs.Items)

		if failedJobsHistoryLimit != nil {
			w.cleanUpOldestJobs(ownedFailedJobs, *failedJobsHistoryLimit, batchv1.JobFailed)
		}

		if successfulJobsHistoryLimit != nil {
			w.cleanUpOldestJobs(ownedCompletedJobs, *successfulJobsHistoryLimit, batchv1.JobComplete)
		}
	}
}

func (w *QueueWorker) cleanUpOldestJobs(jobs []batchv1.Job, limit int32, conditionType batchv1.JobConditionType) {
	oldestJobs := GetOldestJobs(jobs, limit)
	oldJobCount := len(oldestJobs)

	if oldJobCount > 0 {
		w.infof("Cleaning up [%v] of [%v] [%s] jobs for limit [%v]", oldJobCount, len(jobs), conditionType, limit)

		for _, job := range oldestJobs {
			jobNamespace := job.Namespace
			jobName := job.Name

			w.infof("Deleting [%s] job [%s/%s]", conditionType, jobNamespace, jobName)
			err := w.clientset.BatchV1().Jobs(jobNamespace).Delete(jobName, nil)

			if err != nil {
				w.errorf("Could not delete job [%s/%s]: %s", jobNamespace, jobName, err)
			}
		}
	}
}

func (w *QueueWorker) createWorkerLogPrefix() string {
	objectMeta := w.queueResource.GetObjectMeta()
	return fmt.Sprintf("[%s/%s]: ", objectMeta.Namespace, objectMeta.Name)
}

func (w *QueueWorker) DeleteQueueEntryPendingJob(job *batchv1.Job) {
	jobQueueEntryKey := job.Annotations[jobQueueEntryKeyAnnotationKey]
	jobQueueScope := job.Annotations[jobQueueScopeAnnotationKey]

	jobMatchesQueueEntry := false

	if jobQueueEntryKey != "" && w.scope == jobQueueScope {
		queueEntryInfo, hasPendingJob := w.queueEntriesPendingJob[jobQueueEntryKey]

		if hasPendingJob {
			// in case an old failed job for the same ticket is updated/deleted
			jobMatchesQueueEntry = (queueEntryInfo.JobUid == job.UID || queueEntryInfo.JobUid == "")
		}
	}

	if jobMatchesQueueEntry {
		w.infof("Removing queue entry [%s] pending job [%s/%s]", jobQueueEntryKey, job.Namespace, job.Name)
		delete(w.queueEntriesPendingJob, jobQueueEntryKey)
	} else {
		w.infof("Not removing queue entry [%s], doesn't match current pending jobs", jobQueueEntryKey)
	}
}

func (w *QueueWorker) errorf(format string, args ...interface{}) {
	prefix := w.createWorkerLogPrefix()
	glog.Errorf(prefix+format, args...)
}

func (w *QueueWorker) findRecoverableActiveJob(entryKey string) (bool, *batchv1.Job, error) {
	namespace := w.queueResource.GetObjectMeta().Namespace
	jobs, err := w.jobLister.Jobs(namespace).List(labels.Everything())
	if err != nil {
		return false, &batchv1.Job{}, err
	}

	found := false
	recoverableJob := &batchv1.Job{}

	for _, job := range jobs {
		match := (entryKey == job.Annotations[jobQueueEntryKeyAnnotationKey])
		match = (match && (w.scope == job.Annotations[jobQueueScopeAnnotationKey]))
		active := (job.Status.Active == 1)

		if match && active {
			recoverableJob = job
			break
		}
	}

	return found, recoverableJob, nil
}

func (w *QueueWorker) getOwnedFinishedJobs(jobs []batchv1.Job) (ownedCompletedJobs []batchv1.Job, ownedFailedJobs []batchv1.Job) {
	for _, job := range jobs {
		if len(job.OwnerReferences) == 1 {
			jobOwnerReference := job.OwnerReferences[0]

			jobOwnerKind := jobOwnerReference.Kind
			jobOwnerNamespace := job.Namespace
			jobOwnerName := jobOwnerReference.Name

			if jobOwnerKind == w.queueResourceKind &&
				jobOwnerNamespace == w.queueResource.GetObjectMeta().Namespace &&
				jobOwnerName == w.queueResource.GetObjectMeta().Name {

				if finished, conditionType := utils.GetJobFinishedStatus(job); finished {
					if conditionType == batchv1.JobComplete {
						ownedCompletedJobs = append(ownedCompletedJobs, job)
					} else if conditionType == batchv1.JobFailed {
						ownedFailedJobs = append(ownedFailedJobs, job)
					}
				}
			}
		}
	}

	return ownedCompletedJobs, ownedFailedJobs
}

func (w *QueueWorker) GetQueueEntriesPendingJob() map[string]QueueEntryInfo {
	return w.queueEntriesPendingJob
}

func (w *QueueWorker) infof(format string, args ...interface{}) {
	prefix := w.createWorkerLogPrefix()
	glog.Infof(prefix+format, args...)
}

func (w *QueueWorker) processAllQueueEntries() {
	for w.processNextQueueEntry() {
	}
}

func (w *QueueWorker) processEntryInfo(entryInfo QueueEntryInfo) error {
	resourceNamespace := entryInfo.QueueResourceNamespace
	resourceName := entryInfo.QueueResourceName

	queueResource := w.queueResource

	if queueResource.GetSuspend() {
		w.infof("Skipping processing of entry [%s] as [%s/%s] is now marked suspended",
			entryInfo.EntryKey,
			resourceNamespace,
			resourceName)
	} else {
		job := createJobFromTemplate(entryInfo, queueResource)

		job, err := w.clientset.BatchV1().Jobs(resourceNamespace).Create(job)
		if err != nil {
			return err
		}

		jobName := fmt.Sprintf("%s/%s", job.Namespace, job.Name)
		entryInfo.JobUid = job.UID

		message := fmt.Sprintf("Created Job [%s]", jobName)
		w.eventRecorder.Event(queueResource, corev1.EventTypeNormal, "CreatedJob", message)
	}

	return nil
}

func (w *QueueWorker) processNextQueueEntry() bool {
	if int64(len(w.queueEntriesPendingJob)) >= w.queueResource.GetEntryCapacity() {
		return false
	}

	queueWorkerKey := GetQueueWorkerKey(w)

	w.infof("Checking for [%s] queue entries", queueWorkerKey)
	obj, shutdown := w.workqueue.Get()
	if shutdown {
		return false
	}

	defer w.workqueue.Done(obj)

	entryInfo, ok := obj.(QueueEntryInfo)
	if !ok {
		w.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("Expected QueueEntryInfo in [%s] workqueue but got [%#v]", queueWorkerKey, obj))
		return true
	}

	entryKey := entryInfo.EntryKey

	w.infof("Found entry [%s] in work queue", entryKey)

	if err := w.processEntryInfo(entryInfo); err != nil {
		err := fmt.Errorf("Error processing [%s] entry [%s]: %s", queueWorkerKey, entryKey, err.Error())
		runtime.HandleError(err)
		return true
	}

	w.workqueue.Forget(obj)
	w.infof("Successfully processed [%s] entry [%s]", queueWorkerKey, entryKey)

	return true
}

func (w *QueueWorker) queueEntries() {
	queueWorkerKey := GetQueueWorkerKey(w)
	w.infof("Fetching queue entries for [%s]", queueWorkerKey)

	entryKeys, err := w.queueProvider.GetQueueEntryKeys()
	if err != nil {
		runtime.HandleError(err)
		return
	}

	w.queueEntryKeys(entryKeys)
}

func (w *QueueWorker) queueEntryKeys(entryKeys []string) {
	queueWorkerKey := GetQueueWorkerKey(w)
	entryCount := len(entryKeys)
	w.infof("Found [%v] queue entries for [%s]", entryCount, queueWorkerKey)

	queuedEntryCount := 0
	recoveredEntryCount := 0
	skippedEntryCount := 0

	queueResourceName := w.queueResource.GetObjectMeta().Name
	queueResourceNamespace := w.queueResource.GetObjectMeta().Namespace

	for _, entryKey := range entryKeys {
		if _, isAlreadyPendingJob := w.queueEntriesPendingJob[entryKey]; !isAlreadyPendingJob {
			entryInfo := QueueEntryInfo{
				QueueResourceName:      queueResourceName,
				QueueResourceNamespace: queueResourceNamespace,
				EntryKey:               entryKey,
			}

			hasRecoverableJob, recoverableJob, err := w.findRecoverableActiveJob(entryKey)
			if err == nil {
				if hasRecoverableJob {
					w.infof("Recovering job [%s/%s] for queue entry [%s]",
						recoverableJob.Namespace,
						recoverableJob.Name,
						entryKey)

					entryInfo.JobUid = recoverableJob.UID
					recoveredEntryCount++
				} else {
					w.infof("Queueing entry [%s]", entryKey)
					w.workqueue.AddRateLimited(entryInfo)
					queuedEntryCount++
				}
			} else {
				skippedEntryCount++
				err := fmt.Errorf("[%s] caught error checking for recoverable job for [%s/%s] with key [%s]",
					queueWorkerKey,
					queueResourceNamespace,
					queueResourceName,
					entryKey)
				runtime.HandleError(err)
			}

			w.queueEntriesPendingJob[entryKey] = entryInfo
		} else {
			skippedEntryCount++
		}
	}

	w.infof("Queued [%v], recovered [%v], and skipped [%v] entries pending jobs", queuedEntryCount, recoveredEntryCount, skippedEntryCount)
}

func (w *QueueWorker) Run(stopCh <-chan struct{}) {
	w.infof("Starting sub-processes")
	defer w.workqueue.ShutDown()

	pollIntervalSeconds := w.queueResource.GetPollIntervalSeconds()
	if pollIntervalSeconds <= 0 {
		pollIntervalSeconds = 60
	}
	pollIntervalDuration := time.Duration(pollIntervalSeconds)

	go wait.Until(w.queueEntries, pollIntervalDuration*time.Second, stopCh)
	go wait.Until(w.processAllQueueEntries, time.Second, stopCh)
	go wait.Until(w.cleanUp, 10*time.Second, stopCh)

	<-stopCh
}
