package controller

import (
	"context"
	"github.com/fabric8-services/fabric8-build/app"
	"github.com/fabric8-services/fabric8-build/application"
	"github.com/fabric8-services/fabric8-build/application/env"
	"github.com/fabric8-services/fabric8-build/build"
	"github.com/fabric8-services/fabric8-common/errors"
	"github.com/fabric8-services/fabric8-common/httpsupport"
	"github.com/fabric8-services/fabric8-common/token"
	"github.com/goadesign/goa"
	guuid "github.com/goadesign/goa/uuid"
	errs "github.com/pkg/errors"
	"github.com/prometheus/common/log"
)

// PipelineEnvironmentController implements the PipelineEnvironment resource.
type PipelineEnvironmentController struct {
	*goa.Controller
	db         application.DB
	svcFactory application.ServiceFactory
}

// NewPipelineEnvironmentController creates a PipelineEnvironment controller.
func NewPipelineEnvironmentController(service *goa.Service, db application.DB, svcFactory application.ServiceFactory) *PipelineEnvironmentController {
	return &PipelineEnvironmentController{
		Controller: service.NewController("PipelineEnvironmentController"),
		db:         db,
		svcFactory: svcFactory,
	}
}

// Create runs the create action.
func (c *PipelineEnvironmentController) Create(ctx *app.CreatePipelineEnvironmentsContext) error {
	tokenMgr, err := token.ReadManagerFromContext(ctx)
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}
	_, err = tokenMgr.Locate(ctx)
	if err != nil {
		return app.JSONErrorResponse(ctx, errors.NewUnauthorizedError(err.Error()))
	}

	err = validateCreatePipelineEnvironment(ctx)
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	reqPpl := ctx.Payload.Data
	spaceID := ctx.SpaceID
	err = c.checkSpaceExist(ctx, spaceID.String())
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	newEnvs, err := c.checkEnvironmentExistAndConvert(ctx, spaceID.String(), reqPpl.Environments)
	if err != nil {
		return app.JSONErrorResponse(ctx, errors.NewNotFoundError("environment", err.Error()))
	}

	var ppl *build.Pipeline
	err = application.Transactional(c.db, func(appl application.Application) error {
		newPipeline := build.Pipeline{
			Name:        &reqPpl.Name,
			SpaceID:     &spaceID,
			Environment: newEnvs,
		}

		ppl, err = appl.Pipeline().Create(ctx, &newPipeline)
		if err != nil {
			log.Error(ctx, map[string]interface{}{"err": err},
				"failed to create pipeline: %s", newPipeline.Name)
			return errs.Wrapf(err, "failed to create pipeline: %s", *newPipeline.Name)
		}

		return nil
	})

	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	newEnvAttributes := []*app.EnvironmentAttributes{}
	for _, pipeline := range ppl.Environment {
		newEnvAttributes = append(newEnvAttributes, &app.EnvironmentAttributes{
			EnvUUID: pipeline.EnvironmentID,
		})
	}

	res := &app.PipelineEnvironmentSingle{
		Data: &app.PipelineEnvironments{
			ID:           &ppl.ID,
			Name:         *ppl.Name,
			Environments: newEnvAttributes,
			SpaceID:      ppl.SpaceID,
		},
	}

	ctx.ResponseData.Header().Set(
		"Location",
		httpsupport.AbsoluteURL(&goa.RequestData{Request: ctx.Request}, app.PipelineEnvironmentsHref(res.Data.ID), nil),
	)
	return ctx.Created(res)
}

// List runs the list action.
func (c *PipelineEnvironmentController) List(ctx *app.ListPipelineEnvironmentsContext) error {
	spaceID := ctx.SpaceID
	err := c.checkSpaceExist(ctx, spaceID.String())
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	pplenv, err := c.db.Pipeline().List(ctx, spaceID)
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	newPipelineList := []*app.PipelineEnvironments{}
	for _, ppl := range pplenv {
		newPipelineList = append(newPipelineList, convertToPipelineEnvironmentStruct(ppl))
	}

	res := &app.PipelineEnvironmentsList{
		Data: newPipelineList,
	}
	return ctx.OK(res)
}

// Show runs the load action.
func (c *PipelineEnvironmentController) Show(ctx *app.ShowPipelineEnvironmentsContext) error {
	envID := ctx.ID
	ppl, err := c.db.Pipeline().Load(ctx, envID)
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	data := convertToPipelineEnvironmentStruct(ppl)
	res := &app.PipelineEnvironmentSingle{
		Data: data,
	}
	return ctx.OK(res)
}

// Update runs the save action.
func (c *PipelineEnvironmentController) Update(ctx *app.UpdatePipelineEnvironmentsContext) error {
	tokenMgr, err := token.ReadManagerFromContext(ctx)
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}
	_, err = tokenMgr.Locate(ctx)
	if err != nil {
		return app.JSONErrorResponse(ctx, errors.NewUnauthorizedError(err.Error()))
	}

	err = validateUpdatePipelineEnvironment(ctx)
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	reqPpl := ctx.Payload.Data
	spaceID := reqPpl.SpaceID
	err = c.checkSpaceExist(ctx, spaceID.String())
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	newEnvs, err := c.checkEnvironmentExistAndConvert(ctx, spaceID.String(), reqPpl.Environments)
	if err != nil {
		return app.JSONErrorResponse(ctx, errors.NewNotFoundError("environment", err.Error()))
	}

	var ppl *build.Pipeline
	err = application.Transactional(c.db, func(appl application.Application) error {
		ppl, err = c.db.Pipeline().Load(ctx, ctx.ID)
		if err != nil {
			return app.JSONErrorResponse(ctx, err)
		}

		ppl.Name = &ctx.Payload.Data.Name
		ppl.Environment = newEnvs
		ppl, err = appl.Pipeline().Save(ctx, ppl)
		return err
	})
	if err != nil {
		return app.JSONErrorResponse(ctx, err)
	}

	data := convertToPipelineEnvironmentStruct(ppl)
	res := &app.PipelineEnvironmentSingle{
		Data: data,
	}
	return ctx.OK(res)
}

// This will check whether the given space exist or not
func (c *PipelineEnvironmentController) checkSpaceExist(ctx context.Context, spaceID string) error {
	// TODO(chmouel): Make sure we have the rights for that space
	// TODO(chmouel): Better error reporting when NOTFound
	_, err := c.svcFactory.WITService().GetSpace(ctx, spaceID)
	if err != nil {
		return errs.Wrapf(err, "failed to get space id: %s from wit", spaceID)
	}
	return nil
}

// This will check whether the env's exit and then convert to build.Environment List
func (c *PipelineEnvironmentController) checkEnvironmentExistAndConvert(ctx context.Context, spaceID string, envs []*app.EnvironmentAttributes) ([]build.Environment, error) {
	envList, err := c.svcFactory.ENVService().GetEnvList(ctx, spaceID)
	if err != nil {
		return nil, errs.Wrapf(err, "failed to get env list for space id: %s from env service", spaceID)
	}
	envUUIDList := convertToEnvUIDList(envList)
	var environments []build.Environment
	for _, env := range envs {
		envID, _ := guuid.FromString(env.EnvUUID.String())
		envName := envUUIDList[envID]
		if envName == "" {
			return nil, errors.NewNotFoundError("environment", env.EnvUUID.String())
		}
		environments = append(environments, build.Environment{
			EnvironmentID: env.EnvUUID,
		})
	}
	return environments, nil
}

// This will convert the list of env into map[envId]envName
// this will help to check whether env exist or not
func convertToEnvUIDList(envList []env.Environment) map[guuid.UUID]string {
	var envMap = make(map[guuid.UUID]string)
	for _, env := range envList {
		envMap[env.ID] = env.Name
	}
	return envMap
}

// this will convert the pipeline struct from database to pipeline-environment struct
func convertToPipelineEnvironmentStruct(ppl *build.Pipeline) *app.PipelineEnvironments {
	newEnvAttributes := []*app.EnvironmentAttributes{}
	for _, pipeline := range ppl.Environment {
		newEnvAttributes = append(newEnvAttributes, &app.EnvironmentAttributes{
			EnvUUID: pipeline.EnvironmentID,
		})
	}

	pe := &app.PipelineEnvironments{
		ID:           &ppl.ID,
		Name:         *ppl.Name,
		Environments: newEnvAttributes,
		SpaceID:      ppl.SpaceID,
	}
	return pe
}

func validateCreatePipelineEnvironment(ctx *app.CreatePipelineEnvironmentsContext) error {
	if ctx.Payload.Data == nil {
		return errors.NewBadParameterError("data", nil).Expected("not nil")
	}
	if ctx.Payload.Data.SpaceID == nil {
		return errors.NewBadParameterError("data.spaceId", nil).Expected("not nil")
	}
	if ctx.Payload.Data.Name == "" {
		return errors.NewBadParameterError("data.name", nil).Expected("not nil")
	}
	if ctx.Payload.Data.Environments == nil || len(ctx.Payload.Data.Environments) == 0 {
		return errors.NewBadParameterError("data.environments", nil).Expected("not nil")
	}
	return nil
}

func validateUpdatePipelineEnvironment(ctx *app.UpdatePipelineEnvironmentsContext) error {
	if ctx.Payload.Data == nil {
		return errors.NewBadParameterError("data", nil).Expected("not nil")
	}
	if ctx.Payload.Data.SpaceID == nil {
		return errors.NewBadParameterError("data.spaceId", nil).Expected("not nil")
	}
	if ctx.Payload.Data.Name == "" {
		return errors.NewBadParameterError("data.name", nil).Expected("not nil")
	}
	if ctx.Payload.Data.Environments == nil || len(ctx.Payload.Data.Environments) == 0 {
		return errors.NewBadParameterError("data.environments", nil).Expected("not nil")
	}
	return nil
}
