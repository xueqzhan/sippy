// These types are used to decode information from ci-search, but we don't want to expose these for anyone else.
package loader

import (
	gojira "github.com/andygrunwald/go-jira"
	bugsv1 "github.com/openshift/sippy/pkg/apis/bugs/v1"
)

type Search struct {
	Results Results `json:"results"`
}

// search string is the key
type Results map[string]Result

type Result struct {
	Matches []Match `json:"matches"`
}

type Match struct {
	Bug bugsv1.Bug `json:"bugInfo"`
	// Context contains a list of all matched instances for the search regex
	Context []string `json:"context"`
	// Issues contains data on the jira issue. While plural, it can only contain one result from search.ci.
	Issues gojira.Issue `json:"issues"`
}
