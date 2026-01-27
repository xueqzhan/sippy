package query

import (
	"strings"
	"testing"

	"github.com/openshift/sippy/pkg/apis/api/componentreport/crtest"
	"github.com/openshift/sippy/pkg/apis/api/componentreport/reqopts"
	bqcachedclient "github.com/openshift/sippy/pkg/bigquery"
	"github.com/openshift/sippy/pkg/util/sets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildComponentReportQuery_MassFailureCounting(t *testing.T) {
	mockClient := &bqcachedclient.Client{
		Dataset: "test_dataset",
	}

	allJobVariants := crtest.JobVariants{
		Variants: map[string][]string{
			"Platform": {"aws", "gcp"},
			"Network":  {"sdn", "ovn"},
		},
	}

	baseReqOptions := reqopts.RequestOptions{
		VariantOption: reqopts.Variants{
			ColumnGroupBy:   sets.NewString("Platform"),
			DBGroupBy:       sets.NewString("Network"),
			IncludeVariants: map[string][]string{},
		},
		AdvancedOption: reqopts.Advanced{
			IgnoreDisruption: true,
		},
	}

	includeVariants := map[string][]string{}

	tests := []struct {
		name                   string
		exclusiveTestNames     []string
		expectedCTE            bool
		expectedMassFailureCol bool
		expectedParam          bool
		expectedCTEContent     string
	}{
		{
			name:                   "No exclusive tests - mass_failure_count is 0",
			exclusiveTestNames:     nil,
			expectedCTE:            false,
			expectedMassFailureCol: true, // Should still have column but set to 0
			expectedParam:          false,
		},
		{
			name:                   "Empty exclusive tests - mass_failure_count is 0",
			exclusiveTestNames:     []string{},
			expectedCTE:            false,
			expectedMassFailureCol: true,
			expectedParam:          false,
		},
		{
			name: "With exclusive tests - mass failures counted",
			exclusiveTestNames: []string{
				"[sig-cluster-lifecycle] Cluster completes upgrade",
				"install should succeed: overall",
			},
			expectedCTE:            true,
			expectedMassFailureCol: true,
			expectedParam:          true,
			expectedCTEContent:     "jobs_with_failed_exclusive_tests",
		},
		{
			name: "Single exclusive test - mass failures counted",
			exclusiveTestNames: []string{
				"install should succeed: overall",
			},
			expectedCTE:            true,
			expectedMassFailureCol: true,
			expectedParam:          true,
			expectedCTEContent:     "jobs_with_failed_exclusive_tests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqOptions := baseReqOptions
			reqOptions.AdvancedOption.ExclusiveTestNames = tt.exclusiveTestNames

			commonQuery, _, queryParams := BuildComponentReportQuery(
				mockClient,
				reqOptions,
				allJobVariants,
				includeVariants,
				DefaultJunitTable,
				false,
				tt.exclusiveTestNames...,
			)

			// Check if CTE is present when expected
			if tt.expectedCTE {
				assert.Contains(t, commonQuery, "jobs_with_failed_exclusive_tests",
					"Query should contain jobs_with_failed_exclusive_tests CTE")
				assert.Contains(t, commonQuery, "AND success_val = 0",
					"CTE should only identify jobs where exclusive tests FAILED (success_val = 0)")
				assert.Contains(t, commonQuery, "test_name IN UNNEST(@ExclusiveTestNames)",
					"CTE should filter by exclusive test names")
			} else {
				assert.NotContains(t, commonQuery, "jobs_with_failed_exclusive_tests",
					"Query should not contain jobs_with_failed_exclusive_tests CTE when no exclusive tests")
			}

			// Check if mass_failure_count column is present
			if tt.expectedMassFailureCol {
				assert.Contains(t, commonQuery, "mass_failure_count",
					"Query should include mass_failure_count column")

				if len(tt.exclusiveTestNames) > 0 {
					// Should have the CASE logic for counting mass failures
					assert.Contains(t, commonQuery, "prowjob_build_id IN (SELECT prowjob_build_id FROM jobs_with_failed_exclusive_tests)",
						"mass_failure_count should check if job had failed exclusive tests")
					assert.Contains(t, commonQuery, "test_name NOT IN UNNEST(@ExclusiveTestNames)",
						"mass_failure_count should exclude the exclusive tests themselves")
				} else {
					// Should be set to 0
					assert.Contains(t, commonQuery, "0 AS mass_failure_count",
						"mass_failure_count should be 0 when no exclusive tests")
				}
			}

			// Check if query parameter is present when expected
			if tt.expectedParam {
				foundParam := false
				for _, param := range queryParams {
					if param.Name == "ExclusiveTestNames" {
						foundParam = true
						assert.Equal(t, tt.exclusiveTestNames, param.Value,
							"ExclusiveTestNames parameter should match input")
						break
					}
				}
				assert.True(t, foundParam, "ExclusiveTestNames parameter should be present in query parameters")
			} else {
				for _, param := range queryParams {
					assert.NotEqual(t, "ExclusiveTestNames", param.Name,
						"ExclusiveTestNames parameter should not be present when no exclusive tests")
				}
			}

			// Verify CTE content structure if CTE is expected
			if tt.expectedCTEContent != "" {
				// Extract the CTE section
				parts := strings.Split(commonQuery, "latest_component_mapping")
				require.Greater(t, len(parts), 1, "Query should contain latest_component_mapping CTE")

				cteSection := parts[0]
				assert.Contains(t, cteSection, tt.expectedCTEContent,
					"Query should contain expected CTE")

				// Verify the CTE only selects prowjob_build_id for jobs where exclusive tests failed
				// Use a more lenient check that ignores whitespace variations
				normalizedCTE := strings.ReplaceAll(strings.ReplaceAll(cteSection, "\t", " "), "\n", " ")
				assert.Contains(t, normalizedCTE, "DISTINCT prowjob_build_id",
					"CTE should select distinct prowjob_build_id")
			}
		})
	}
}

func TestBuildComponentReportQuery_MassFailureCountLogic(t *testing.T) {
	// This test verifies the specific logic: we count failures from jobs
	// where exclusive tests FAILED as mass_failure_count
	mockClient := &bqcachedclient.Client{
		Dataset: "test_dataset",
	}

	allJobVariants := crtest.JobVariants{
		Variants: map[string][]string{
			"Platform": {"aws"},
		},
	}

	reqOptions := reqopts.RequestOptions{
		VariantOption: reqopts.Variants{
			ColumnGroupBy:   sets.NewString("Platform"),
			DBGroupBy:       sets.NewString(),
			IncludeVariants: map[string][]string{},
		},
		AdvancedOption: reqopts.Advanced{
			ExclusiveTestNames: []string{"install should succeed: overall"},
		},
	}

	commonQuery, _, _ := BuildComponentReportQuery(
		mockClient,
		reqOptions,
		allJobVariants,
		map[string][]string{},
		DefaultJunitTable,
		false,
		"install should succeed: overall",
	)

	// The query should:
	// 1. Create a CTE that identifies jobs where exclusive tests FAILED
	assert.Contains(t, commonQuery, "WITH jobs_with_failed_exclusive_tests AS",
		"Should create CTE for failed exclusive tests")

	// 2. The CTE should check success_val = 0 (failure)
	cteEnd := strings.Index(commonQuery, "latest_component_mapping")
	require.Greater(t, cteEnd, 0, "Should contain latest_component_mapping CTE")

	cteSection := commonQuery[:cteEnd]
	assert.Contains(t, cteSection, "success_val = 0",
		"CTE should only match FAILED exclusive tests (success_val = 0), not all instances")

	// 3. Count mass failures using the CTE
	assert.Contains(t, commonQuery, "mass_failure_count",
		"Should include mass_failure_count column")

	// 4. Verify the mass_failure_count logic
	// Normalize whitespace for easier parsing
	normalizedQuery := strings.ReplaceAll(strings.ReplaceAll(commonQuery, "\t", " "), "\n", " ")
	normalizedQuery = strings.Join(strings.Fields(normalizedQuery), " ") // Collapse all whitespace

	// Should count failures (adjusted_success_val = 0) from jobs with failed exclusive tests
	assert.Contains(t, normalizedQuery, "adjusted_success_val = 0",
		"Should count failed tests")
	assert.Contains(t, normalizedQuery, "prowjob_build_id IN (SELECT prowjob_build_id FROM jobs_with_failed_exclusive_tests)",
		"Should check if test is from a job with failed exclusive tests")
	assert.Contains(t, normalizedQuery, "test_name NOT IN UNNEST(@ExclusiveTestNames)",
		"Should not count exclusive tests themselves as mass failures")
}

func TestBuildComponentReportQuery_WithAndWithoutExclusiveTests(t *testing.T) {
	// This test compares queries with and without exclusive tests to ensure
	// mass_failure_count handling is correct
	mockClient := &bqcachedclient.Client{
		Dataset: "test_dataset",
	}

	allJobVariants := crtest.JobVariants{
		Variants: map[string][]string{
			"Platform": {"aws"},
		},
	}

	baseReqOptions := reqopts.RequestOptions{
		VariantOption: reqopts.Variants{
			ColumnGroupBy:   sets.NewString("Platform"),
			DBGroupBy:       sets.NewString(),
			IncludeVariants: map[string][]string{},
		},
		AdvancedOption: reqopts.Advanced{},
	}

	// Query without exclusive tests
	queryWithout, _, paramsWithout := BuildComponentReportQuery(
		mockClient,
		baseReqOptions,
		allJobVariants,
		map[string][]string{},
		DefaultJunitTable,
		false,
	)

	// Query with exclusive tests
	reqOptionsWithExclusive := baseReqOptions
	reqOptionsWithExclusive.AdvancedOption.ExclusiveTestNames = []string{"install should succeed: overall"}

	queryWith, _, paramsWith := BuildComponentReportQuery(
		mockClient,
		reqOptionsWithExclusive,
		allJobVariants,
		map[string][]string{},
		DefaultJunitTable,
		false,
		"install should succeed: overall",
	)

	// Both should have the component_mapping CTE
	assert.Contains(t, queryWithout, "latest_component_mapping",
		"Query without exclusive tests should have component mapping CTE")
	assert.Contains(t, queryWith, "latest_component_mapping",
		"Query with exclusive tests should have component mapping CTE")

	// Both should have mass_failure_count column
	assert.Contains(t, queryWithout, "mass_failure_count",
		"Query without exclusive tests should have mass_failure_count column")
	assert.Contains(t, queryWith, "mass_failure_count",
		"Query with exclusive tests should have mass_failure_count column")

	// Query without exclusive tests should set mass_failure_count to 0
	assert.Contains(t, queryWithout, "0 AS mass_failure_count",
		"Query without exclusive tests should set mass_failure_count to 0")

	// Only the query with exclusive tests should have the CTE for identifying failed jobs
	assert.NotContains(t, queryWithout, "jobs_with_failed_exclusive_tests",
		"Query without exclusive tests should not have jobs_with_failed_exclusive_tests CTE")
	assert.Contains(t, queryWith, "jobs_with_failed_exclusive_tests",
		"Query with exclusive tests should have jobs_with_failed_exclusive_tests CTE")

	// Check parameters
	assert.Len(t, paramsWithout, 0, "Query without exclusive tests should have no extra parameters")
	assert.Len(t, paramsWith, 1, "Query with exclusive tests should have 1 parameter")
	if len(paramsWith) > 0 {
		assert.Equal(t, "ExclusiveTestNames", paramsWith[0].Name,
			"Parameter should be named ExclusiveTestNames")
	}
}
