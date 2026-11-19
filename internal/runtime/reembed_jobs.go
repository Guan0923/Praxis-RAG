package runtime

import (
	"context"
	"errors"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// SubmitReembed validates the new settings and records the rebuild independently
// of the administrator's HTTP request. Settings use the same credential
// protection as the live knowledge-base configuration.
func (s *Service) SubmitReembed(ctx context.Context, namespace string, input kbconfig.UpdateInput) (*state.ReembedJob, error) {
	snapshot, err := s.stores.KnowledgeBases.Snapshot(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	if input.EncryptionKey == "" {
		input.EncryptionKey = s.stores.Settings.EncryptionKey
	}
	input.Reembed = true
	result, err := kbconfig.UpdateSettings(ctx, snapshot.Settings, input, kbconfig.HTTPProber{})
	if err != nil {
		return nil, updateError(err)
	}
	collection, err := settingString(snapshot.Settings, "collection")
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识库索引配置无效"}
	}
	job, err := s.stores.ReembedJobs.Submit(ctx, namespace, snapshot.Version, collection, result.Settings)
	var conflict *state.StateConflict
	if errors.As(err, &conflict) {
		return nil, &apierrors.KnowledgeValidationError{Message: conflict.Message}
	}
	return job, err
}

func (s *Service) LatestReembed(ctx context.Context, namespace string) (*state.ReembedJob, error) {
	return s.stores.ReembedJobs.Latest(ctx, namespace)
}

// ProcessReembed runs under the same namespace lock as synchronous re-embedding.
// Retried delivery reloads the job after acquiring the lock, so completed jobs
// are no-ops and a rebuild interrupted by worker exit starts with a clean target.
func (s *Service) ProcessReembed(ctx context.Context, namespace, id string) error {
	jobs := s.stores.ReembedJobs
	task, err := jobs.Pending(ctx, namespace, id)
	if err != nil || task == nil {
		return err
	}
	return s.stores.Uploads.WithLock(ctx, namespace, nil, func(token *state.LockToken) error {
		task, err := jobs.Pending(ctx, namespace, id)
		if err != nil || task == nil {
			return err
		}
		return s.rebuildKnowledgeBaseIndex(ctx, token, task)
	})
}

func (s *Service) rebuildKnowledgeBaseIndex(ctx context.Context, token *state.LockToken, task *state.ReembedTask) error {
	snapshot, err := s.stores.KnowledgeBases.Snapshot(ctx, task.Namespace)
	if err != nil {
		return err
	}
	if snapshot == nil || snapshot.Version != task.BaseVersion || snapshot.Settings["collection"] != task.BaseCollection {
		return s.failReembed(ctx, token, task, state.ErrReembedSourceChanged.Error())
	}
	embedding, err := kbconfig.ModelConnectionForKey(task.TargetSettings, "embedding", s.stores.Settings.EncryptionKey)
	if err != nil {
		return s.failReembed(ctx, token, task, "无法读取任务的 Embedding 模型配置")
	}

	staging, err := kbconfig.NewCollectionName()
	if err != nil {
		return err
	}
	s.discardCollection(task.StagingCollection)
	if err := s.stores.ReembedJobs.Start(ctx, token, task, staging); err != nil {
		return err
	}
	task.StagingCollection = staging
	if err := s.reembedIntoStaging(ctx, task.Namespace, snapshot.Settings, staging, kbconfig.UpdateResult{Embedding: embedding}, task.BaseVersion); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.failReembed(ctx, token, task, "重新向量化失败，请检查 Embedding 模型服务和 Qdrant 状态后重新提交")
	}

	if err := s.stores.Uploads.RenewLock(ctx, token); err != nil {
		return err
	}
	settings := kbconfig.ActivateReembed(task.TargetSettings, staging, s.now())
	err = s.stores.ReembedJobs.Publish(ctx, token, task, settings)
	if errors.Is(err, state.ErrReembedSourceChanged) {
		return s.failReembed(ctx, token, task, err.Error())
	}
	return err
}

func (s *Service) failReembed(ctx context.Context, token *state.LockToken, task *state.ReembedTask, message string) error {
	if err := s.stores.ReembedJobs.Fail(ctx, token, task, message); err != nil {
		return err
	}
	s.discardCollection(task.StagingCollection)
	return nil
}
