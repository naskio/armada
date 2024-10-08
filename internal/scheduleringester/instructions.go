package scheduleringester

import (
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"

	"github.com/armadaproject/armada/internal/common/armadacontext"
	"github.com/armadaproject/armada/internal/common/compress"
	"github.com/armadaproject/armada/internal/common/ingest"
	"github.com/armadaproject/armada/internal/common/ingest/metrics"
	protoutil "github.com/armadaproject/armada/internal/common/proto"
	"github.com/armadaproject/armada/internal/scheduler/adapters"
	schedulerdb "github.com/armadaproject/armada/internal/scheduler/database"
	"github.com/armadaproject/armada/internal/scheduler/schedulerobjects"
	"github.com/armadaproject/armada/internal/server/configuration"
	"github.com/armadaproject/armada/pkg/armadaevents"
)

type eventSequenceCommon struct {
	queue  string
	jobset string
	user   string
	groups []string
}

type InstructionConverter struct {
	metrics    *metrics.Metrics
	compressor compress.Compressor
}

func NewInstructionConverter(
	metrics *metrics.Metrics,
) (*InstructionConverter, error) {
	compressor, err := compress.NewZlibCompressor(1024)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create compressor")
	}
	return &InstructionConverter{
		metrics:    metrics,
		compressor: compressor,
	}, nil
}

func (c *InstructionConverter) Convert(_ *armadacontext.Context, sequencesWithIds *ingest.EventSequencesWithIds) *DbOperationsWithMessageIds {
	operations := make([]DbOperation, 0)
	for _, es := range sequencesWithIds.EventSequences {
		for _, op := range c.dbOperationsFromEventSequence(es) {
			operations = AppendDbOperation(operations, op)
		}
	}
	log.Infof("Converted sequences into %d db operations", len(operations))
	return &DbOperationsWithMessageIds{
		Ops:        operations,
		MessageIds: sequencesWithIds.MessageIds,
	}
}

func (c *InstructionConverter) dbOperationsFromEventSequence(es *armadaevents.EventSequence) []DbOperation {
	meta := eventSequenceCommon{
		queue:  es.Queue,
		jobset: es.JobSetName,
		user:   es.UserId,
		groups: es.Groups,
	}
	operations := make([]DbOperation, 0, len(es.Events))
	for idx, event := range es.Events {
		eventTime := protoutil.ToStdTime(event.Created)
		var err error = nil
		var operationsFromEvent []DbOperation
		switch eventType := event.GetEvent().(type) {
		case *armadaevents.EventSequence_Event_SubmitJob:
			operationsFromEvent, err = c.handleSubmitJob(event.GetSubmitJob(), eventTime, meta)
		case *armadaevents.EventSequence_Event_JobRunLeased:
			operationsFromEvent, err = c.handleJobRunLeased(event.GetJobRunLeased(), eventTime, meta)
		case *armadaevents.EventSequence_Event_JobRunRunning:
			operationsFromEvent, err = c.handleJobRunRunning(event.GetJobRunRunning(), eventTime)
		case *armadaevents.EventSequence_Event_JobRunSucceeded:
			operationsFromEvent, err = c.handleJobRunSucceeded(event.GetJobRunSucceeded(), eventTime)
		case *armadaevents.EventSequence_Event_JobRunErrors:
			operationsFromEvent, err = c.handleJobRunErrors(event.GetJobRunErrors(), eventTime)
		case *armadaevents.EventSequence_Event_JobSucceeded:
			operationsFromEvent, err = c.handleJobSucceeded(event.GetJobSucceeded())
		case *armadaevents.EventSequence_Event_JobErrors:
			operationsFromEvent, err = c.handleJobErrors(event.GetJobErrors())
		case *armadaevents.EventSequence_Event_JobPreemptionRequested:
			operationsFromEvent, err = c.handleJobPreemptionRequested(event.GetJobPreemptionRequested(), meta)
		case *armadaevents.EventSequence_Event_ReprioritiseJob:
			operationsFromEvent, err = c.handleReprioritiseJob(event.GetReprioritiseJob(), meta)
		case *armadaevents.EventSequence_Event_ReprioritiseJobSet:
			operationsFromEvent, err = c.handleReprioritiseJobSet(event.GetReprioritiseJobSet(), meta)
		case *armadaevents.EventSequence_Event_CancelJob:
			operationsFromEvent, err = c.handleCancelJob(event.GetCancelJob(), meta)
		case *armadaevents.EventSequence_Event_CancelJobSet:
			operationsFromEvent, err = c.handleCancelJobSet(event.GetCancelJobSet(), meta)
		case *armadaevents.EventSequence_Event_CancelledJob:
			operationsFromEvent, err = c.handleCancelledJob(event.GetCancelledJob(), eventTime)
		case *armadaevents.EventSequence_Event_JobRequeued:
			operationsFromEvent, err = c.handleJobRequeued(event.GetJobRequeued())
		case *armadaevents.EventSequence_Event_PartitionMarker:
			operationsFromEvent, err = c.handlePartitionMarker(event.GetPartitionMarker(), eventTime)
		case *armadaevents.EventSequence_Event_JobRunPreempted:
			operationsFromEvent, err = c.handleJobRunPreempted(event.GetJobRunPreempted(), eventTime)
		case *armadaevents.EventSequence_Event_JobRunAssigned:
			operationsFromEvent, err = c.handleJobRunAssigned(event.GetJobRunAssigned(), eventTime)
		case *armadaevents.EventSequence_Event_JobValidated:
			operationsFromEvent, err = c.handleJobValidated(event.GetJobValidated())
		case *armadaevents.EventSequence_Event_ReprioritisedJob,
			*armadaevents.EventSequence_Event_ResourceUtilisation,
			*armadaevents.EventSequence_Event_JobRunCancelled,
			*armadaevents.EventSequence_Event_StandaloneIngressInfo:
			// These events can all be safely ignored
			log.Debugf("Ignoring event type %T", event)
		default:
			// This is an event type we haven't considered. Log a warning
			log.Warnf("Ignoring unknown event type %T", eventType)
		}
		if err != nil {
			c.metrics.RecordPulsarMessageError(metrics.PulsarMessageErrorProcessing)
			log.WithError(err).Errorf("Could not convert event at index %d.", idx)
		} else {
			operations = append(operations, operationsFromEvent...)
		}
	}
	return operations
}

func (c *InstructionConverter) handleSubmitJob(job *armadaevents.SubmitJob, submitTime time.Time, meta eventSequenceCommon) ([]DbOperation, error) {
	jobId := job.JobIdStr

	// Store the job submit message so that it can be sent to an executor.
	submitJobBytes, err := proto.Marshal(job)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	compressedSubmitJobBytes, err := c.compressor.Compress(submitJobBytes)
	if err != nil {
		return nil, err
	}

	// Produce a minimal representation of the job for the scheduler.
	// To avoid the scheduler needing to load the entire job spec.
	schedulingInfo, err := c.schedulingInfoFromSubmitJob(job, submitTime)
	if err != nil {
		return nil, err
	}
	schedulingInfoBytes, err := proto.Marshal(schedulingInfo)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	compressedGroups, err := compress.CompressStringArray(meta.groups, c.compressor)
	if err != nil {
		return nil, err
	}

	return []DbOperation{InsertJobs{jobId: &schedulerdb.Job{
		JobID:                 jobId,
		JobSet:                meta.jobset,
		UserID:                meta.user,
		Groups:                compressedGroups,
		Queue:                 meta.queue,
		Queued:                true,
		QueuedVersion:         0,
		Submitted:             submitTime.UnixNano(),
		Priority:              int64(job.Priority),
		SubmitMessage:         compressedSubmitJobBytes,
		SchedulingInfo:        schedulingInfoBytes,
		SchedulingInfoVersion: int32(schedulingInfo.Version),
	}}}, nil
}

func (c *InstructionConverter) handleJobRunLeased(jobRunLeased *armadaevents.JobRunLeased, eventTime time.Time, meta eventSequenceCommon) ([]DbOperation, error) {
	runId := jobRunLeased.RunIdStr
	var scheduledAtPriority *int32
	if jobRunLeased.HasScheduledAtPriority {
		scheduledAtPriority = &jobRunLeased.ScheduledAtPriority
	}
	PodRequirementsOverlay, err := proto.Marshal(jobRunLeased.GetPodRequirementsOverlay())
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return []DbOperation{
		InsertRuns{runId: &JobRunDetails{
			Queue: meta.queue,
			DbRun: &schedulerdb.Run{
				RunID:                  runId,
				JobID:                  jobRunLeased.JobIdStr,
				Created:                eventTime.UnixNano(),
				JobSet:                 meta.jobset,
				Queue:                  meta.queue,
				Executor:               jobRunLeased.GetExecutorId(),
				Node:                   jobRunLeased.GetNodeId(),
				Pool:                   jobRunLeased.GetPool(),
				ScheduledAtPriority:    scheduledAtPriority,
				LeasedTimestamp:        &eventTime,
				PodRequirementsOverlay: PodRequirementsOverlay,
			},
		}},
		UpdateJobQueuedState{jobRunLeased.JobIdStr: &JobQueuedStateUpdate{
			Queued:             false,
			QueuedStateVersion: jobRunLeased.UpdateSequenceNumber,
		}},
	}, nil
}

func (c *InstructionConverter) handleJobRequeued(jobRequeued *armadaevents.JobRequeued) ([]DbOperation, error) {
	schedulingInfoBytes, err := proto.Marshal(jobRequeued.SchedulingInfo)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return []DbOperation{
		UpdateJobQueuedState{jobRequeued.JobIdStr: &JobQueuedStateUpdate{
			Queued:             true,
			QueuedStateVersion: jobRequeued.UpdateSequenceNumber,
		}},
		UpdateJobSchedulingInfo{jobRequeued.JobIdStr: &JobSchedulingInfoUpdate{
			JobSchedulingInfo:        schedulingInfoBytes,
			JobSchedulingInfoVersion: int32(jobRequeued.SchedulingInfo.Version),
		}},
	}, nil
}

func (c *InstructionConverter) handleJobRunRunning(jobRunRunning *armadaevents.JobRunRunning, runningTime time.Time) ([]DbOperation, error) {
	runId := jobRunRunning.RunIdStr
	return []DbOperation{MarkRunsRunning{runId: runningTime}}, nil
}

func (c *InstructionConverter) handleJobRunSucceeded(jobRunSucceeded *armadaevents.JobRunSucceeded, successTime time.Time) ([]DbOperation, error) {
	runId := jobRunSucceeded.RunIdStr
	return []DbOperation{MarkRunsSucceeded{runId: successTime}}, nil
}

func (c *InstructionConverter) handleJobRunPreempted(jobRunPreempted *armadaevents.JobRunPreempted, preemptedTime time.Time) ([]DbOperation, error) {
	runId := jobRunPreempted.PreemptedRunIdStr
	return []DbOperation{MarkRunsPreempted{runId: preemptedTime}}, nil
}

func (c *InstructionConverter) handleJobRunAssigned(jobRunAssigned *armadaevents.JobRunAssigned, assignedTime time.Time) ([]DbOperation, error) {
	runId := jobRunAssigned.RunIdStr
	return []DbOperation{MarkRunsPending{runId: assignedTime}}, nil
}

func (c *InstructionConverter) handleJobRunErrors(jobRunErrors *armadaevents.JobRunErrors, failureTime time.Time) ([]DbOperation, error) {
	runId := jobRunErrors.RunIdStr
	jobId := jobRunErrors.JobIdStr
	insertJobRunErrors := make(InsertJobRunErrors)
	markRunsFailed := make(MarkRunsFailed)
	for _, runError := range jobRunErrors.GetErrors() {
		// There should only be one terminal error.
		if runError.GetTerminal() {
			bytes, err := protoutil.MarshallAndCompress(runError, c.compressor)
			if err != nil {
				return nil, errors.Wrap(err, "failed to marshal RunError")
			}
			insertJobRunErrors[runId] = &schedulerdb.JobRunError{
				RunID: runId,
				JobID: jobId,
				Error: bytes,
			}
			runAttempted := true
			if runError.GetPodLeaseReturned() != nil {
				runAttempted = runError.GetPodLeaseReturned().RunAttempted
			}
			markRunsFailed[runId] = &JobRunFailed{
				LeaseReturned: runError.GetPodLeaseReturned() != nil,
				RunAttempted:  runAttempted,
				FailureTime:   failureTime,
			}
			return []DbOperation{insertJobRunErrors, markRunsFailed}, nil
		}
	}
	return nil, nil
}

func (c *InstructionConverter) handleJobSucceeded(jobSucceeded *armadaevents.JobSucceeded) ([]DbOperation, error) {
	return []DbOperation{MarkJobsSucceeded{
		jobSucceeded.JobIdStr: true,
	}}, nil
}

func (c *InstructionConverter) handleJobErrors(jobErrors *armadaevents.JobErrors) ([]DbOperation, error) {
	for _, jobError := range jobErrors.GetErrors() {
		// For terminal errors, we also need to mark the job as failed.
		if jobError.GetTerminal() {
			markJobsFailed := make(MarkJobsFailed)
			markJobsFailed[jobErrors.JobIdStr] = true
			return []DbOperation{markJobsFailed}, nil
		}
	}
	return nil, nil
}

func (c *InstructionConverter) handleJobPreemptionRequested(preemptionRequested *armadaevents.JobPreemptionRequested, meta eventSequenceCommon) ([]DbOperation, error) {
	return []DbOperation{MarkRunsForJobPreemptRequested{
		JobSetKey{
			queue:  meta.queue,
			jobSet: meta.jobset,
		}: []string{preemptionRequested.JobIdStr},
	}}, nil
}

func (c *InstructionConverter) handleReprioritiseJob(reprioritiseJob *armadaevents.ReprioritiseJob, meta eventSequenceCommon) ([]DbOperation, error) {
	return []DbOperation{&UpdateJobPriorities{
		key: JobReprioritiseKey{
			JobSetKey: JobSetKey{
				queue:  meta.queue,
				jobSet: meta.jobset,
			},
			Priority: int64(reprioritiseJob.Priority),
		},
		jobIds: []string{reprioritiseJob.JobIdStr},
	}}, nil
}

func (c *InstructionConverter) handleReprioritiseJobSet(reprioritiseJobSet *armadaevents.ReprioritiseJobSet, meta eventSequenceCommon) ([]DbOperation, error) {
	return []DbOperation{UpdateJobSetPriorities{
		JobSetKey{queue: meta.queue, jobSet: meta.jobset}: int64(reprioritiseJobSet.Priority),
	}}, nil
}

func (c *InstructionConverter) handleCancelJob(cancelJob *armadaevents.CancelJob, meta eventSequenceCommon) ([]DbOperation, error) {
	return []DbOperation{MarkJobsCancelRequested{
		JobSetKey{
			queue:  meta.queue,
			jobSet: meta.jobset,
		}: []string{cancelJob.JobIdStr},
	}}, nil
}

func (c *InstructionConverter) handleCancelJobSet(cancelJobSet *armadaevents.CancelJobSet, meta eventSequenceCommon) ([]DbOperation, error) {
	cancelQueued := len(cancelJobSet.States) == 0 || slices.Contains(cancelJobSet.States, armadaevents.JobState_QUEUED)
	cancelLeased := len(cancelJobSet.States) == 0 || slices.Contains(cancelJobSet.States, armadaevents.JobState_PENDING) || slices.Contains(cancelJobSet.States, armadaevents.JobState_RUNNING)

	return []DbOperation{MarkJobSetsCancelRequested{
		JobSetKey{
			queue:  meta.queue,
			jobSet: meta.jobset,
		}: &JobSetCancelAction{
			cancelQueued: cancelQueued,
			cancelLeased: cancelLeased,
		},
	}}, nil
}

func (c *InstructionConverter) handleCancelledJob(cancelledJob *armadaevents.CancelledJob, cancelTime time.Time) ([]DbOperation, error) {
	return []DbOperation{MarkJobsCancelled{
		cancelledJob.JobIdStr: cancelTime,
	}}, nil
}

func (c *InstructionConverter) handlePartitionMarker(pm *armadaevents.PartitionMarker, created time.Time) ([]DbOperation, error) {
	return []DbOperation{&InsertPartitionMarker{
		markers: []*schedulerdb.Marker{
			{
				GroupID:     armadaevents.UuidFromProtoUuid(pm.GroupId),
				PartitionID: int32(pm.Partition),
				Created:     created,
			},
		},
	}}, nil
}

func (c *InstructionConverter) handleJobValidated(checked *armadaevents.JobValidated) ([]DbOperation, error) {
	return []DbOperation{
		MarkJobsValidated{checked.JobIdStr: checked.Pools},
	}, nil
}

// schedulingInfoFromSubmitJob returns a minimal representation of a job containing only the info needed by the scheduler.
func (c *InstructionConverter) schedulingInfoFromSubmitJob(submitJob *armadaevents.SubmitJob, submitTime time.Time) (*schedulerobjects.JobSchedulingInfo, error) {
	return SchedulingInfoFromSubmitJob(submitJob, submitTime)
}

// SchedulingInfoFromSubmitJob returns a minimal representation of a job containing only the info needed by the scheduler.
func SchedulingInfoFromSubmitJob(submitJob *armadaevents.SubmitJob, submitTime time.Time) (*schedulerobjects.JobSchedulingInfo, error) {
	// Component common to all jobs.
	schedulingInfo := &schedulerobjects.JobSchedulingInfo{
		Lifetime:        submitJob.Lifetime,
		AtMostOnce:      submitJob.AtMostOnce,
		Preemptible:     submitJob.Preemptible,
		ConcurrencySafe: submitJob.ConcurrencySafe,
		SubmitTime:      submitTime,
		Priority:        submitJob.Priority,
		Version:         0,
	}

	// Scheduling requirements specific to the objects that make up this job.
	switch object := submitJob.MainObject.Object.(type) {
	case *armadaevents.KubernetesMainObject_PodSpec:
		podSpec := object.PodSpec.PodSpec
		schedulingInfo.PriorityClassName = podSpec.PriorityClassName
		podRequirements := adapters.PodRequirementsFromPodSpec(podSpec)
		if submitJob.ObjectMeta != nil {
			podRequirements.Annotations = maps.Clone(submitJob.ObjectMeta.Annotations)
		}
		if submitJob.MainObject.ObjectMeta != nil {
			if podRequirements.Annotations == nil {
				podRequirements.Annotations = make(map[string]string, len(submitJob.MainObject.ObjectMeta.Annotations))
			}
			for k, v := range submitJob.MainObject.ObjectMeta.Annotations {
				if configuration.IsSchedulingAnnotation(k) {
					podRequirements.Annotations[k] = v
				}
			}
		}
		schedulingInfo.ObjectRequirements = append(
			schedulingInfo.ObjectRequirements,
			&schedulerobjects.ObjectRequirements{
				Requirements: &schedulerobjects.ObjectRequirements_PodRequirements{
					PodRequirements: podRequirements,
				},
			},
		)
	default:
		return nil, errors.Errorf("unsupported object type %T", object)
	}
	return schedulingInfo, nil
}
