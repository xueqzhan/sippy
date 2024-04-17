package main

import (
	"context"
	"github.com/openshift/sippy/pkg/api"
	apitype "github.com/openshift/sippy/pkg/apis/api"
	"io/fs"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	resources "github.com/openshift/sippy"
	"github.com/openshift/sippy/pkg/apis/cache"
	v1 "github.com/openshift/sippy/pkg/apis/config/v1"
	"github.com/openshift/sippy/pkg/bigquery"
	"github.com/openshift/sippy/pkg/dataloader/prowloader/gcs"
	"github.com/openshift/sippy/pkg/flags"
	"github.com/openshift/sippy/pkg/sippyserver"
	"github.com/openshift/sippy/pkg/sippyserver/metrics"
)

type ComponentReadinessFlags struct {
	GoogleCloudFlags *flags.GoogleCloudFlags
	BigQueryFlags    *flags.BigQueryFlags
	CacheFlags       *flags.CacheFlags

	Config      string
	LogLevel    string
	ListenAddr  string
	MetricsAddr string
	RedisURL    string
}

func NewComponentReadinessCommand() *cobra.Command {
	f := &ComponentReadinessFlags{
		LogLevel:    "info",
		ListenAddr:  ":8080",
		MetricsAddr: ":2112",

		GoogleCloudFlags: flags.NewGoogleCloudFlags(),
		BigQueryFlags:    flags.NewBigQueryFlags(),
		CacheFlags:       flags.NewCacheFlags(),
	}

	cmd := &cobra.Command{
		Use: "component-readiness",

		RunE: func(cmd *cobra.Command, arguments []string) error {

			if err := f.Validate(); err != nil {
				return errors.WithMessage(err, "error validating options")
			}
			if err := f.Run(); err != nil {
				return errors.WithMessage(err, "error running command")
			}

			return nil
		},
	}

	f.BindFlags(cmd.Flags())

	return cmd
}

func (f *ComponentReadinessFlags) BindFlags(flagSet *pflag.FlagSet) {
	f.CacheFlags.BindFlags(flagSet)
	f.BigQueryFlags.BindFlags(flagSet)
	f.GoogleCloudFlags.BindFlags(flagSet)
	flagSet.StringVar(&f.LogLevel, "log-level", f.LogLevel, "Log level (trace,debug,info,warn,error) (default info)")
	flagSet.StringVar(&f.ListenAddr, "listen", f.ListenAddr, "The address to serve analysis reports on (default :8080)")
	flagSet.StringVar(&f.MetricsAddr, "listen-metrics", f.MetricsAddr, "The address to serve prometheus metrics on (default :2112)")
}

func (f *ComponentReadinessFlags) Validate() error {
	return nil
}

func (f *ComponentReadinessFlags) Run() error { //nolint:gocyclo
	// Set log level
	level, err := log.ParseLevel(f.LogLevel)
	if err != nil {
		log.WithError(err).Fatal("Cannot parse log-level")
	}
	log.SetLevel(level)

	// Add some millisecond precision to log timestamps, useful for debugging performance.
	formatter := new(log.TextFormatter)
	formatter.TimestampFormat = "2006-01-02T15:04:05.999Z07:00"
	formatter.FullTimestamp = true
	formatter.DisableColors = false
	log.SetFormatter(formatter)

	log.Debug("debug logging enabled")
	sippyConfig := v1.SippyConfig{}
	if f.Config == "" {
		sippyConfig.Prow = v1.ProwConfig{
			URL: "https://prow.ci.openshift.org/prowjobs.js",
		}
	} else {
		data, err := os.ReadFile(f.Config)
		if err != nil {
			log.WithError(err).Fatalf("could not load config")
		}
		if err := yaml.Unmarshal(data, &sippyConfig); err != nil {
			log.WithError(err).Fatalf("could not unmarshal config")
		}
	}

	return f.runServerMode()
}

func (f *ComponentReadinessFlags) runServerMode() error {
	var err error

	webRoot, err := fs.Sub(resources.SippyNG, "sippy-ng/build")
	if err != nil {
		log.WithError(err).Fatal("could not load frontend")
	}

	cacheClient, err := f.CacheFlags.GetCacheClient()
	if err != nil {
		return errors.WithMessage(err, "couldn't get cache client")
	}

	var bigQueryClient *bigquery.Client
	var gcsClient *storage.Client
	if f.GoogleCloudFlags.ServiceAccountCredentialFile != "" {
		bigQueryClient, err = f.BigQueryFlags.GetBigQueryClient(context.Background(),
			cacheClient, f.GoogleCloudFlags.ServiceAccountCredentialFile)
		if err != nil {
			return errors.WithMessage(err, "couldn't get bigquery client")
		}

		gcsClient, err = gcs.NewGCSClient(context.TODO(),
			f.GoogleCloudFlags.ServiceAccountCredentialFile,
			f.GoogleCloudFlags.OAuthClientCredentialFile,
		)
		if err != nil {
			log.WithError(err).Warn("unable to create GCS client, some APIs may not work")
		}
	}

	crVariants := apitype.ComponentReportTestVariants2{Variants: map[string][]string{}}
	errs := []error{}
	if bigQueryClient != nil {
		crVariants, errs = api.GetComponentTestVariantsFromBigQuery2(bigQueryClient, f.GoogleCloudFlags.StorageBucket)
		if len(errs) != 0 {
			log.Warningf("%d errors were encountered while querying for component report variants", len(errs))
			for _, err := range errs {
				log.Error(err.Error())
			}
			return errors.WithMessage(err, "couldn't get component report variants")
		}
	}

	server := sippyserver.NewServer(
		sippyserver.ModeOpenShift,
		f.ListenAddr,
		nil,
		nil,
		webRoot,
		&resources.Static,
		nil,
		f.GoogleCloudFlags.StorageBucket,
		gcsClient,
		bigQueryClient,
		nil,
		cacheClient,
		4*time.Hour,
		crVariants,
	)

	if f.MetricsAddr != "" {
		// Do an immediate metrics update
		err = metrics.RefreshMetricsDB(nil,
			bigQueryClient,
			f.GoogleCloudFlags.StorageBucket,
			nil,
			time.Time{},
			cache.RequestOptions{CRTimeRoundingFactor: defaultCRTimeRoundingFactor})
		if err != nil {
			log.WithError(err).Error("error refreshing metrics")
		}

		// Refresh our metrics every 5 minutes:
		ticker := time.NewTicker(5 * time.Minute)
		quit := make(chan struct{})
		go func() {
			for {
				select {
				case <-ticker.C:
					log.Info("tick")
					err := metrics.RefreshMetricsDB(nil, bigQueryClient, f.GoogleCloudFlags.StorageBucket, nil, time.Time{}, cache.RequestOptions{CRTimeRoundingFactor: defaultCRTimeRoundingFactor})
					if err != nil {
						log.WithError(err).Error("error refreshing metrics")
					}
				case <-quit:
					ticker.Stop()
					return
				}
			}
		}()

		// Serve our metrics endpoint for prometheus to scrape
		go func() {
			http.Handle("/metrics", promhttp.Handler())
			err := http.ListenAndServe(f.MetricsAddr, nil) //nolint
			if err != nil {
				panic(err)
			}
		}()
	}

	server.Serve()
	return nil
}
