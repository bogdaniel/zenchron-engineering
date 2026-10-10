package product

// ProductConfiguration (#476): the product's MUTABLE settings, kept separate
// from the frozen configuration and policy identities a run or plan already
// bound into its own ContractProvenance (domain.ContractProvenance).
//
// "Mutable" here means an operator may adopt a new revision at any time, not
// that any one revision is mutable: each revision is written once and kept,
// exactly like a WorkGraph or EngineeringPlan revision. A run that resolved
// Policy or ContextPolicy from revision N keeps referencing revision N's exact
// values forever; adopting revision N+1 never rewrites what N said.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ConfigurationSchemaVersion versions the durable configuration revision document.
const ConfigurationSchemaVersion = "0.1"

// MaxConfigurationSettings bounds one revision's free-form settings.
const MaxConfigurationSettings = 32

const (
	maxSettingKeyBytes   = 100
	maxSettingValueBytes = 1 << 10
)

// ProductConfiguration is one revision of a product's effective configuration.
type ProductConfiguration struct {
	SchemaVersion string `json:"schema_version"`
	ProductID     string `json:"product_id"`
	Revision      int    `json:"revision"`
	// Policy is the product's default EngineeringPolicy reference. It is a
	// reference only - #476 never compiles or stores policy content, and this
	// field grants no authority of its own: a run still resolves its own
	// policy the way it already does.
	Policy *domain.ObjectRevision `json:"policy,omitempty"`
	// ContextPolicy is the product's default #64 ContextPolicy reference, by
	// that object's own content-addressed identity.
	ContextPolicy string            `json:"context_policy,omitempty"`
	Settings      map[string]string `json:"settings,omitempty"`
	RequestedBy   string            `json:"requested_by,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// RevisionDigest is the CONTENT identity of one revision.
func (c ProductConfiguration) RevisionDigest() (string, error) {
	return domain.Digest(struct {
		ProductID     string                 `json:"product_id"`
		Revision      int                    `json:"revision"`
		Policy        *domain.ObjectRevision `json:"policy,omitempty"`
		ContextPolicy string                 `json:"context_policy,omitempty"`
		Settings      map[string]string      `json:"settings,omitempty"`
	}{c.ProductID, c.Revision, c.Policy, c.ContextPolicy, c.Settings})
}

// Validate is the complete deterministic admission check for one revision
// document, independent of any other revision.
func (c ProductConfiguration) Validate() error {
	if c.SchemaVersion != ConfigurationSchemaVersion {
		return fmt.Errorf("product configuration schema version %q is not %q", c.SchemaVersion, ConfigurationSchemaVersion)
	}
	if err := boundedField("configuration product id", c.ProductID, maxProductNameBytes); err != nil {
		return err
	}
	if c.Revision < 1 {
		return fmt.Errorf("product configuration revision %d is not a revision", c.Revision)
	}
	if c.CreatedAt.IsZero() {
		return errors.New("product configuration creation time is required")
	}
	if c.Policy != nil {
		if strings.TrimSpace(c.Policy.ID) == "" || strings.TrimSpace(c.Policy.Revision) == "" {
			return errors.New("product configuration policy reference needs an id and a revision")
		}
	}
	if len(c.Settings) > MaxConfigurationSettings {
		return fmt.Errorf("product configuration names %d settings, above the %d setting bound", len(c.Settings), MaxConfigurationSettings)
	}
	for key, value := range c.Settings {
		if err := boundedField("configuration setting key", key, maxSettingKeyBytes); err != nil {
			return err
		}
		if len(value) > maxSettingValueBytes {
			return fmt.Errorf("configuration setting %q value is %d bytes, above the %d byte bound", key, len(value), maxSettingValueBytes)
		}
	}
	return nil
}

// ValidateConfigurationMutation is the deterministic gate a proposed next
// configuration revision passes before anything adopts it. Unlike a Product's
// repositories, settings carry no append-only constraint: configuration is
// the MUTABLE half of #476 by design, and what must never move is the
// historical revision itself, which this function's caller (the store) keeps
// by never overwriting an already-adopted revision row.
func ValidateConfigurationMutation(current, next ProductConfiguration) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.ProductID != current.ProductID {
		return fmt.Errorf("configuration revision %d describes a different product than %s", next.Revision, current.ProductID)
	}
	if next.Revision != current.Revision+1 {
		return fmt.Errorf("product %s configuration is at revision %d; the next revision is %d, not %d",
			current.ProductID, current.Revision, current.Revision+1, next.Revision)
	}
	return nil
}
