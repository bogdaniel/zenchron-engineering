package analysis

import (
	"fmt"
	"os"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// LoadProjectModel loads a strict, schema-validated ProjectModel snapshot.
func LoadProjectModel(path string) (domain.ProjectModel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.ProjectModel{}, fmt.Errorf("read ProjectModel snapshot: %w", err)
	}
	model, err := domain.Decode[domain.ProjectModel](data)
	if err != nil {
		return domain.ProjectModel{}, fmt.Errorf("load ProjectModel snapshot: %w", err)
	}
	return model, nil
}
