package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"time"

	"github.com/fabric8-services/fabric8-build/app"
	"github.com/fabric8-services/fabric8-build/application"
	"github.com/fabric8-services/fabric8-build/configuration"
	"github.com/fabric8-services/fabric8-build/controller"
	"github.com/fabric8-services/fabric8-build/gormapp"
	"github.com/fabric8-services/fabric8-build/migration"
	"github.com/fabric8-services/fabric8-common/goamiddleware"
	"github.com/fabric8-services/fabric8-common/log"
	"github.com/fabric8-services/fabric8-common/metric"
	"github.com/fabric8-services/fabric8-common/sentry"
	"github.com/fabric8-services/fabric8-common/token"
	"github.com/goadesign/goa"
	goalogrus "github.com/goadesign/goa/logging/logrus"
	"github.com/goadesign/goa/middleware"
	"github.com/goadesign/goa/middleware/gzip"
	"github.com/google/gops/agent"
	"github.com/jinzhu/gorm"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {

	// --------------------------------------------------------------------
	// Parse flags
	// --------------------------------------------------------------------
	var configFilePath string
	var printConfig bool
	var migrateDB bool
	flag.StringVar(&configFilePath, "config", "", "Path to the config file to read")
	flag.BoolVar(&printConfig, "printConfig", false, "Prints the config (including merged environment variables) and exits")
	flag.BoolVar(&migrateDB, "migrateDatabase", false, "Migrates the database to the newest version and exits.")
	flag.Parse()

	// Override default -config switch with environment variable only if -config switch was
	// not explicitly given via the command line.
	configSwitchIsSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configSwitchIsSet = true
		}
	})
	if !configSwitchIsSet {
		if envConfigPath, ok := os.LookupEnv("F8_CONFIG_FILE_PATH"); ok {
			configFilePath = envConfigPath
		}
	}

	config, err := configuration.New(configFilePath)
	if err != nil {
		log.Panic(context.TODO(), map[string]interface{}{
			"config_file_path": configFilePath,
			"err":              err,
		}, "failed to setup the configuration")
	}

	if printConfig {
		os.Exit(0)
	}

	// Initialized developer mode flag and log level for the logger
	log.InitializeLogger(config.IsLogJSON(), config.GetLogLevel())

	db := connect(config)
	defer func() {
		err = db.Close()
		if err != nil {
			log.Panic(context.TODO(), map[string]interface{}{
				"err": err,
			}, "failure to close db connexion")
		}
	}()

	err = migration.Migrate(db.DB(), config.GetPostgresDatabase())
	if err != nil {
		log.Panic(context.TODO(), map[string]interface{}{
			"err": err,
		}, "failed migration")
	}
	if migrateDB {
		os.Exit(0)
	}

	// Initialize sentry client
	haltSentry, err := sentry.InitializeSentryClient(
		nil, // will use the `os.Getenv("Sentry_DSN")` instead
		sentry.WithRelease(app.Commit),
		sentry.WithEnvironment(config.GetEnvironment()),
	)
	if err != nil {
		log.Panic(context.TODO(), map[string]interface{}{
			"err": err,
		}, "failed to setup the sentry client")
	}
	defer haltSentry()

	printUserInfo()

	// Create service
	service := goa.New("fabric8-build")

	// Mount middleware
	service.Use(middleware.RequestID())
	// Use our own log request to inject identity id and modify other properties
	service.Use(gzip.Middleware(9))
	service.Use(app.ErrorHandler(service, true))
	service.Use(middleware.Recover())

	// create a token manager and use
	tokenMgr := getTokenManager(config)
	tokenCtxMW := goamiddleware.TokenContext(tokenMgr, app.NewJWTSecurity())
	service.Use(tokenCtxMW)
	service.Use(token.InjectTokenManager(tokenMgr))

	// Create the service factory
	svcFactory := application.NewServiceFactory(config)

	// record HTTP request metrics in prometh
	service.Use(
		metric.Recorder(
			"fabric8_build_service",
			metric.WithRequestDurationBucket(prometheus.ExponentialBuckets(0.05, 2, 8))))
	service.WithLogger(goalogrus.New(log.Logger()))
	// service.Use(metric.Recorder())

	// Mount the 'status' controller
	statusCtrl := controller.NewStatusController(service)
	app.MountStatusController(service, statusCtrl)

	appDB := gormapp.NewGormDB(db)

	// Mount the 'pipeline environment map' controller
	pipelineEnvCtrl := controller.NewPipelineEnvironmentMapsController(service, appDB, svcFactory)
	app.MountPipelineEnvironmentMapsController(service, pipelineEnvCtrl)

	log.Logger().Infoln("Git Commit SHA: ", app.Commit)
	log.Logger().Infoln("UTC Build Time: ", app.BuildTime)
	log.Logger().Infoln("UTC Start Time: ", app.StartTime)
	log.Logger().Infoln("GOMAXPROCS:     ", runtime.GOMAXPROCS(-1))
	log.Logger().Infoln("NumCPU:         ", runtime.NumCPU())

	http.Handle("/api/", service.Mux)
	http.Handle("/favicon.ico", http.NotFoundHandler())

	if config.GetDiagnoseHTTPAddress() != "" {
		log.Logger().Infoln("Diagnose:       ", config.GetDiagnoseHTTPAddress())
		// Start diagnostic http
		if err := agent.Listen(agent.Options{Addr: config.GetDiagnoseHTTPAddress(), ConfigDir: "/tmp/gops/"}); err != nil {
			log.Error(context.TODO(), map[string]interface{}{
				"addr": config.GetDiagnoseHTTPAddress(),
				"err":  err,
			}, "unable to connect to diagnose server")
		}
	}

	// // Start/mount metrics http
	if config.GetHTTPAddress() == config.GetMetricsHTTPAddress() {
		http.Handle("/metrics", promhttp.Handler())
	} else {
		go func(metricAddress string) {
			mx := http.NewServeMux()
			mx.Handle("/metrics", promhttp.Handler())
			if err := http.ListenAndServe(metricAddress, mx); err != nil {
				log.Error(context.TODO(), map[string]interface{}{
					"addr": metricAddress,
					"err":  err,
				}, "unable to connect to metrics server")
				service.LogError("startup", "err", err)
			}
		}(config.GetMetricsHTTPAddress())
	}

	// Start http
	if err := http.ListenAndServe(config.GetHTTPAddress(), nil); err != nil {
		log.Error(context.TODO(), map[string]interface{}{
			"addr": config.GetHTTPAddress(),
			"err":  err,
		}, "unable to connect to server")
		service.LogError("startup", "err", err)
	}

}

func connect(config *configuration.Config) *gorm.DB {
	var err error
	var db *gorm.DB
	for {
		db, err = gorm.Open("postgres", config.GetPostgresConfigString())
		if err != nil {
			log.Logger().Errorf("ERROR: Unable to open connection to database %v", err)
			log.Logger().Infof("Retrying to connect in %v...", config.GetPostgresConnectionRetrySleep())
			time.Sleep(config.GetPostgresConnectionRetrySleep())
		} else {
			break
		}
	}

	if config.DeveloperModeEnabled() {
		db = db.Debug()
	}

	if config.GetPostgresConnectionMaxIdle() > 0 {
		log.Logger().Infof("Configured connection pool max idle %v", config.GetPostgresConnectionMaxIdle())
		db.DB().SetMaxIdleConns(config.GetPostgresConnectionMaxIdle())
	}
	if config.GetPostgresConnectionMaxOpen() > 0 {
		log.Logger().Infof("Configured connection pool max open %v", config.GetPostgresConnectionMaxOpen())
		db.DB().SetMaxOpenConns(config.GetPostgresConnectionMaxOpen())
	}
	return db
}

func getTokenManager(config *configuration.Config) token.Manager {
	tokenMgr, err := token.DefaultManager(config)
	if err != nil {
		log.Panic(nil, map[string]interface{}{"err": err},
			"failed to setup jwt middleware")
	}
	return tokenMgr
}

func printUserInfo() {
	u, err := user.Current()
	if err != nil {
		log.Warn(context.TODO(), map[string]interface{}{
			"err": err,
		}, "failed to get current user")
	} else {
		log.Info(context.TODO(), map[string]interface{}{
			"username": u.Username,
			"uuid":     u.Uid,
		}, "Running as user name '%s' with UID %s.", u.Username, u.Uid)
		g, err := user.LookupGroupId(u.Gid)
		if err != nil {
			log.Warn(context.TODO(), map[string]interface{}{
				"err": err,
			}, "failed to lookup group")
		} else {
			log.Info(context.TODO(), map[string]interface{}{
				"groupname": g.Name,
				"gid":       g.Gid,
			}, "Running as as group '%s' with GID %s.", g.Name, g.Gid)
		}
	}

}
