package erpc

import (
	// "context"

	"context"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/rs/zerolog/log"
)

type ProjectsRegistry struct {
	appCtx context.Context

	rateLimitersRegistry *upstream.RateLimitersRegistry
	evmJsonRpcCache      *EvmJsonRpcCache
	preparedProjects     map[string]*PreparedProject
	staticProjects       []*common.ProjectConfig
	vendorsRegistry      *vendors.VendorsRegistry
}

func NewProjectsRegistry(
	ctx context.Context,
	staticProjects []*common.ProjectConfig,
	evmJsonRpcCache *EvmJsonRpcCache,
	rateLimitersRegistry *upstream.RateLimitersRegistry,
	vendorsRegistry *vendors.VendorsRegistry,
) (*ProjectsRegistry, error) {
	reg := &ProjectsRegistry{
		appCtx:               ctx,
		staticProjects:       staticProjects,
		preparedProjects:     make(map[string]*PreparedProject),
		rateLimitersRegistry: rateLimitersRegistry,
		evmJsonRpcCache:      evmJsonRpcCache,
		vendorsRegistry:      vendorsRegistry,
	}

	for _, prjCfg := range staticProjects {
		_, err := reg.RegisterProject(prjCfg)
		if err != nil {
			return nil, err
		}
	}

	return reg, nil
}

func (r *ProjectsRegistry) GetProject(projectId string) (project *PreparedProject, err error) {
	project, exists := r.preparedProjects[projectId]
	if !exists {
		project, err = r.loadProject(projectId)
	}
	return
}

func (r *ProjectsRegistry) RegisterProject(prjCfg *common.ProjectConfig) (*PreparedProject, error) {
	if _, ok := r.preparedProjects[prjCfg.Id]; ok {
		return nil, common.NewErrProjectAlreadyExists(prjCfg.Id)
	}

	lg := log.Logger.With().Str("project", prjCfg.Id).Logger()

	ws := prjCfg.HealthCheck.ScoreMetricsWindowSize
	if ws == "" {
		ws = "30s"
	}
	wsDuration, err := time.ParseDuration(ws)
	if err != nil {
		return nil, err
	}
	metricsTracker := health.NewTracker(prjCfg.Id, wsDuration)
	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		&lg,
		prjCfg.Id,
		prjCfg.Upstreams,
		r.rateLimitersRegistry,
		r.vendorsRegistry,
		metricsTracker,
	)
	err = upstreamsRegistry.Bootstrap(r.appCtx)
	if err != nil {
		return nil, err
	}
	networksRegistry := NewNetworksRegistry(
		upstreamsRegistry,
		metricsTracker,
		r.evmJsonRpcCache,
		r.rateLimitersRegistry,
	)

	pp := &PreparedProject{
		Config: prjCfg,
		Logger: &lg,

		appCtx:            r.appCtx,
		networksRegistry:  networksRegistry,
		upstreamsRegistry: upstreamsRegistry,
		evmJsonRpcCache:   r.evmJsonRpcCache,
	}
	pp.Networks = make(map[string]*Network)

	r.preparedProjects[prjCfg.Id] = pp

	log.Info().Msgf("registered project %s", prjCfg.Id)

	return pp, nil
}

func (r *ProjectsRegistry) loadProject(projectId string) (*PreparedProject, error) {
	for _, prjCfg := range r.staticProjects {
		if prjCfg.Id == projectId {
			return r.RegisterProject(prjCfg)
		}
	}

	// TODO implement dynamic project config loading from DB

	return nil, common.NewErrProjectNotFound(projectId)
}
