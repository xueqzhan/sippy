package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/openshift/sippy/pkg/api"
	"github.com/openshift/sippy/pkg/api/componentreadiness"
	crtype "github.com/openshift/sippy/pkg/apis/api/componentreport"
	"github.com/openshift/sippy/pkg/apis/cache"
	bqcachedclient "github.com/openshift/sippy/pkg/bigquery"
	"github.com/openshift/sippy/pkg/componentreadiness/jiraautomator"
	"github.com/openshift/sippy/pkg/flags"
	"github.com/openshift/sippy/pkg/util/sets"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	prowflagutil "sigs.k8s.io/prow/pkg/flagutil"
)

type AutomateJiraFlags struct {
	BigQueryFlags           *flags.BigQueryFlags
	GoogleCloudFlags        *flags.GoogleCloudFlags
	CacheFlags              *flags.CacheFlags
	ComponentReadinessFlags *flags.ComponentReadinessFlags
	JiraOptions             prowflagutil.JiraOptions
	SippyURL                string
	IncludeComponentsStr    string
	IncludeComponents       sets.String
	ColumnThresholdStrs     []string
	ColumnThresholds        map[jiraautomator.Variant]int
	JiraAccount             string
	DryRun                  bool
}

func NewAutomateJiraFlags() *AutomateJiraFlags {
	return &AutomateJiraFlags{
		BigQueryFlags:           flags.NewBigQueryFlags(),
		GoogleCloudFlags:        flags.NewGoogleCloudFlags(),
		CacheFlags:              flags.NewCacheFlags(),
		ComponentReadinessFlags: flags.NewComponentReadinessFlags(),
		ColumnThresholds:        map[jiraautomator.Variant]int{},
	}
}

func (f *AutomateJiraFlags) BindFlags(fs *pflag.FlagSet) {
	f.BigQueryFlags.BindFlags(fs)
	f.GoogleCloudFlags.BindFlags(fs)
	f.CacheFlags.BindFlags(fs)
	f.ComponentReadinessFlags.BindFlags(fs)
	f.JiraOptions.AddFlags(flag.CommandLine)
	fs.AddGoFlagSet(flag.CommandLine)
	fs.StringVar(&f.SippyURL, "sippy-url", f.SippyURL, "The Sippy URL prefix to be used to generate sharable Sippy links")
	fs.StringVar(&f.IncludeComponentsStr, "include-components", f.IncludeComponentsStr, "The list of comma separated jira components to file issues against. If this is not defined, all components will be candidates.")
	fs.StringArrayVar(&f.ColumnThresholdStrs, "column-threshold", f.ColumnThresholdStrs, "A threshold of red cell counts over which a jira issue will be created against a component corresponding to an interesting variant of a column (e.g. Bare Metal Hardware Provisioning for metal platform). The format of the threshold string is [variant]:[value]:[threshold] (e.g. Platform:metal:3).")
	fs.StringVar(&f.JiraAccount, "jira-account", f.JiraAccount, "The jira account used to automate jira")
	fs.BoolVar(&f.DryRun, "dry-run", f.DryRun, "Print the tasks of automating jiras without real interaction with jira.")
}

func (f *AutomateJiraFlags) Validate(allVariants crtype.JobVariants) error {
	if len(f.SippyURL) == 0 {
		return fmt.Errorf("--sippy-url is required")
	}
	if len(f.JiraAccount) == 0 {
		return fmt.Errorf("--jira-account is required")
	}
	f.IncludeComponents = sets.NewString(strings.Split(f.IncludeComponentsStr, ",")...)
	for _, variantThreshold := range f.ColumnThresholdStrs {
		vt := strings.Split(variantThreshold, ":")
		if len(vt) != 3 {
			return fmt.Errorf("--variant-based-component-regression-threshold %s is in wrong format", variantThreshold)
		}
		vs, ok := allVariants.Variants[vt[0]]
		if !ok {
			return fmt.Errorf("--variant-based-component-regression-threshold %s has wrong variant name %s", variantThreshold, vt[0])
		}
		found := false
		for _, v := range vs {
			if v == vt[1] {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("--variant-based-component-regression-threshold %s has wrong variant value %s", variantThreshold, vt[1])
		}
		t, err := strconv.Atoi(vt[2])
		if err != nil {
			return fmt.Errorf("--variant-based-component-regression-threshold %s has wrong threshold %s", variantThreshold, vt[2])
		}
		f.ColumnThresholds[jiraautomator.Variant{Name: vt[0], Value: vt[1]}] = t
	}
	return f.JiraOptions.Validate(true)
}

func NewAutomateJiraCommand() *cobra.Command {
	f := NewAutomateJiraFlags()

	cmd := &cobra.Command{
		Use:   "automate-jira",
		Short: "Automate jira with component readiness regressions",
		Long:  "Check the component report for each view with automate jira enabled. Maintains jira cards for current regressions automatically.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour*1)
			defer cancel()

			cacheClient, err := f.CacheFlags.GetCacheClient()
			if err != nil {
				log.WithError(err).Fatal("couldn't get cache client")
			}

			bigQueryClient, err := bqcachedclient.New(ctx,
				f.GoogleCloudFlags.ServiceAccountCredentialFile,
				f.BigQueryFlags.BigQueryProject,
				f.BigQueryFlags.BigQueryDataset, cacheClient)
			if err != nil {
				log.WithError(err).Fatal("CRITICAL error getting BigQuery client which prevents regression tracking")
			}

			cacheOpts := cache.RequestOptions{CRTimeRoundingFactor: f.ComponentReadinessFlags.CRTimeRoundingFactor}

			views, err := f.ComponentReadinessFlags.ParseViewsFile()
			if err != nil {
				log.WithError(err).Fatal("unable to load views")
			}
			releases, err := api.GetReleases(context.Background(), bigQueryClient)
			if err != nil {
				log.WithError(err).Fatal("error querying releases")
			}

			jiraClient, err := f.JiraOptions.Client()
			if err != nil {
				return errors.WithMessage(err, "couldn't get jira client")
			}

			allVariants, errs := componentreadiness.GetJobVariantsFromBigQuery(ctx, bigQueryClient, f.GoogleCloudFlags.StorageBucket)
			if len(errs) > 0 {
				return fmt.Errorf("failed to get variants from bigquery")
			}
			if err := f.Validate(allVariants); err != nil {
				return errors.WithMessage(err, "error validating options")
			}

			j, err := jiraautomator.NewJiraAutomator(jiraClient, bigQueryClient, cacheOpts, views.ComponentReadiness, releases, f.SippyURL, f.JiraAccount, f.IncludeComponents, f.ColumnThresholds, f.DryRun)
			if err != nil {
				panic(err)
			}
			return j.Run()
		},
	}

	f.BindFlags(cmd.Flags())

	return cmd
}