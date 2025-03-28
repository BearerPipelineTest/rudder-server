package app

//go:generate mockgen -destination=../mocks/app/mock_features.go -package=mock_app github.com/rudderlabs/rudder-server/app MigratorFeature,SuppressUserFeature

import (
	"context"

	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/utils/types"
)

// MigratorFeature handles migration of nodes during cluster's scale up/down.
type MigratorFeature interface {
	Run(context.Context, *jobsdb.HandleT, *jobsdb.HandleT, *jobsdb.HandleT, func(), func())
	PrepareJobsdbsForImport(*jobsdb.HandleT, *jobsdb.HandleT, *jobsdb.HandleT)
}

// MigratorFeatureSetup is a function that initializes a Migrator feature, based on application instance
type MigratorFeatureSetup func(Interface) MigratorFeature

// SuppressUserFeature handles webhook event requests
type SuppressUserFeature interface {
	Setup(backendConfig backendconfig.BackendConfig) (types.SuppressUserI, error)
}

/*********************************
DestinationConfig Env Support
*********************************/

// ConfigEnvFeature handles override of config from ENV variables.
type ConfigEnvFeature interface {
	Setup() types.ConfigEnvI
}

/*********************************
Reporting Feature
*********************************/

// ReportingFeature handles reporting statuses / errors to reporting service
type ReportingFeature interface {
	Setup(backendConfig backendconfig.BackendConfig) types.ReportingI
	GetReportingInstance() types.ReportingI
}

/*********************************
Replay Feature
*********************************/

// ReplayFeature handles inserting of failed jobs into respective gw/rt jobsdb
type ReplayFeature interface {
	Setup(ctx context.Context, replayDB, gwDB, routerDB, batchRouterDB *jobsdb.HandleT)
}

// ReplayFeatureSetup is a function that initializes a Replay feature
type ReplayFeatureSetup func(Interface) ReplayFeature

// Features contains optional implementations of Enterprise only features.
type Features struct {
	Migrator     MigratorFeature
	SuppressUser SuppressUserFeature
	ConfigEnv    ConfigEnvFeature
	Reporting    ReportingFeature
	Replay       ReplayFeature
}
