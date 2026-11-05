package metadata

import (
	"errors"
	"fmt"

	"github.com/goccy/go-json"
	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// ErrDuplicatedRoutine is returned when a routine is added to a dataset that
// already holds a routine with the same ID.
var ErrDuplicatedRoutine = errors.New("routine is already created")

type Routine struct {
	ID        string
	ProjectID string
	DatasetID string
	metadata  map[string]interface{}
	repo      *Repository
}

func NewRoutine(repo *Repository, projectID, datasetID, routineID string, metadata map[string]interface{}) *Routine {
	return &Routine{
		ID:        routineID,
		ProjectID: projectID,
		DatasetID: datasetID,
		metadata:  metadata,
		repo:      repo,
	}
}

// Content returns the routine as a REST resource.
func (r *Routine) Content() (*bigqueryv2.Routine, error) {
	encoded, err := json.Marshal(r.metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to encode metadata: %w", err)
	}
	var v bigqueryv2.Routine
	if err := json.Unmarshal(encoded, &v); err != nil {
		return nil, fmt.Errorf("failed to decode metadata to routine: %w", err)
	}
	return &v, nil
}
