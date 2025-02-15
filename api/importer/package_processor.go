package importer

import (
	"fmt"
	"io"

	"github.com/keptn/keptn/api/importer/model"
)

//go:generate moq -pkg fake --skip-ensure -out ./fake/package_processor_mock.go . ImportPackage:ImportPackageMock ManifestParser:ManifestParserMock TaskExecutor:TaskExecutorMock
//go:generate moq -pkg fake --skip-ensure -out ./fake/stage_retriever_mock.go . ProjectStageRetriever:MockStageRetriever
//go:generate moq -pkg fake --skip-ensure -out ./fake/renderer_mock.go . Renderer:MockRenderer

type Renderer interface {
	RenderContent(raw io.ReadCloser, context any) (io.ReadCloser, error)
	RenderString(raw string, context any) (string, error)
}

type ImportPackage interface {
	io.Closer
	GetResource(resourceName string) (io.ReadCloser, error)
}

type ManifestParser interface {
	Parse(input io.Reader) (*model.ImportManifest, error)
}

type TaskExecutor interface {
	ExecuteAPI(ate model.APITaskExecution) (any, error)
	PushResource(rp model.ResourcePush) (any, error)
}

type ProjectStageRetriever interface {
	GetStages(project string) ([]string, error)
}

type ImportPackageProcessor struct {
	parser         ManifestParser
	executor       TaskExecutor
	stageRetriever ProjectStageRetriever
	renderer       Renderer
}

func NewImportPackageProcessor(
	mp ManifestParser, ex TaskExecutor, retriever ProjectStageRetriever,
) *ImportPackageProcessor {
	return newImportPackageProcessor(mp, ex, retriever, &templateRenderer{})
}

func newImportPackageProcessor(
	mp ManifestParser, ex TaskExecutor, retriever ProjectStageRetriever, renderer Renderer,
) *ImportPackageProcessor {
	return &ImportPackageProcessor{
		parser:         mp,
		executor:       ex,
		stageRetriever: retriever,
		renderer:       renderer,
	}
}

const manifestFileName = "manifest.yaml"
const apiTaskType = "api"
const resourceTaskType = "resource"

func (ipp *ImportPackageProcessor) Process(project string, ip ImportPackage) error {

	defer ip.Close()

	manifestReader, err := ip.GetResource(manifestFileName)

	if err != nil {
		return fmt.Errorf("error accessing manifest: %w", err)
	}

	defer manifestReader.Close()

	manifest, err := ipp.parser.Parse(manifestReader)

	if err != nil {
		return fmt.Errorf("error parsing manifest: %w", err)
	}

	mCtx := model.NewManifestExecution(project)

	for _, task := range manifest.Tasks {

		switch task.Type {
		case apiTaskType:
			if err = ipp.processAPITask(mCtx, ip, task); err != nil {
				return err
			}
		case resourceTaskType:
			if err = ipp.processResourceTask(mCtx, ip, task); err != nil {
				return err
			}
		default:
			return fmt.Errorf("task of type %s not implemented", task.Type)
		}
	}

	return nil
}

func (ipp *ImportPackageProcessor) processResourceTask(
	mCtx *model.ManifestExecution, ip ImportPackage, task *model.ManifestTask,
) error {
	if task.ResourceTask == nil {
		return fmt.Errorf("malformed task of type resource: %+v", task)
	}
	var stages []string
	var err error
	if task.ResourceTask.Stage == "" {
		stages, err = ipp.stageRetriever.GetStages(mCtx.GetProject())
		if err != nil {
			return fmt.Errorf("error retrieving stages for project %s: %w", mCtx.GetProject(), err)
		}
	} else {
		stages = []string{task.Stage}
	}

	for _, stage := range stages {
		resourcePush, err := ipp.mapResourcePush(mCtx, stage, ip, task)
		if err != nil {
			return fmt.Errorf("error setting up resource push for task ID %s: %w", task.ID, err)
		}
		response, err := ipp.executor.PushResource(resourcePush)
		if err != nil {
			return fmt.Errorf("resource task id %s failed: %w", task.ID, err)
		}
		// TODO what should we store for multiple stage resources upload ?
		mCtx.Tasks[task.ID] = model.TaskExecution{
			TaskContext: resourcePush.Context,
			Response:    response,
		}
	}
	return nil
}

func (ipp *ImportPackageProcessor) processAPITask(
	mCtx *model.ManifestExecution, ip ImportPackage, task *model.ManifestTask,
) error {
	if task.APITask == nil {
		return fmt.Errorf("malformed task of type api: %+v", task)
	}
	apiTaskExecution, err := ipp.mapAPITask(mCtx, ip, task)
	if err != nil {
		return fmt.Errorf("error setting up API task ID %s: %w", task.ID, err)
	}
	response, err := ipp.executor.ExecuteAPI(apiTaskExecution)
	if err != nil {
		return fmt.Errorf("execution of task %s failed: %w", task.ID, err)
	}

	// store task context and response into the manifest context
	mCtx.Tasks[task.ID] = model.TaskExecution{
		TaskContext: apiTaskExecution.Context,
		Response:    response,
	}

	return nil
}

func (ipp *ImportPackageProcessor) mapResourcePush(
	mCtx *model.ManifestExecution, stage string, ip ImportPackage,
	task *model.ManifestTask,
) (model.ResourcePush,
	error) {
	resource, err := ip.GetResource(task.ResourceTask.File)
	if err != nil {
		return model.ResourcePush{}, fmt.Errorf("error accessing resource content: %w", err)
	}

	taskContext, err := ipp.newTaskContext(mCtx, task)
	if err != nil {
		return model.ResourcePush{}, fmt.Errorf(
			"error building task context for task %s: %w", task.ID,
			err,
		)
	}

	renderedResource, err := ipp.renderer.RenderContent(resource, taskContext)
	if err != nil {
		return model.ResourcePush{}, fmt.Errorf("error rendering resource content: %w", err)
	}

	ret := model.ResourcePush{
		Content:     renderedResource,
		ResourceURI: task.ResourceTask.RemoteURI,
		Stage:       stage,
		Service:     task.ResourceTask.Service,
		Context:     taskContext,
	}

	return ret, nil
}

func (ipp *ImportPackageProcessor) mapAPITask(
	mCtx *model.ManifestExecution, ip ImportPackage,
	task *model.ManifestTask,
) (model.APITaskExecution, error) {

	payload, err := ip.GetResource(task.APITask.PayloadFile)

	if err != nil {
		return model.APITaskExecution{}, fmt.Errorf(
			"error accessing payload %s: %w", task.APITask.PayloadFile,
			err,
		)
	}

	taskContext, err := ipp.newTaskContext(mCtx, task)
	if err != nil {
		return model.APITaskExecution{}, fmt.Errorf(
			"error building task context for task %s: %w", task.ID,
			err,
		)
	}

	renderedPayload, err := ipp.renderer.RenderContent(payload, taskContext)
	if err != nil {
		return model.APITaskExecution{}, fmt.Errorf(
			"error rendering payload %s: %w", task.APITask.PayloadFile,
			err,
		)
	}

	ret := model.APITaskExecution{
		Payload:    renderedPayload,
		EndpointID: task.APITask.Action,
		Context:    taskContext,
	}

	return ret, nil
}

func (ipp *ImportPackageProcessor) newTaskContext(
	mCtx *model.ManifestExecution, task *model.ManifestTask,
) (model.TaskContext, error) {
	renderedContext, err := ipp.renderContext(mCtx, task.Context)

	if err != nil {
		return model.TaskContext{}, fmt.Errorf("error while creating task context: %w", err)
	}

	return model.TaskContext{
		Project: mCtx.GetProject(),
		Task:    task,
		Context: renderedContext,
	}, nil
}

func (ipp *ImportPackageProcessor) renderContext(
	mCtx *model.ManifestExecution, context map[string]string,
) (map[string]string, error) {
	renderedContext := map[string]string{}
	for k, v := range context {
		renderedValue, err := ipp.renderer.RenderString(v, mCtx)
		if err != nil {
			return nil, fmt.Errorf("error rendering value for context key %s: %w", k, err)
		}
		renderedContext[k] = renderedValue
	}
	return renderedContext, nil
}
