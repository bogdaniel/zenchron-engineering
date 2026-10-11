package runtime

// The detached assurance checkout contains only the bound candidate tree.
// Populate the masked, ephemeral Git directory so repository-aware checks can
// enumerate that tree without receiving host Git configuration or history.
// Tests create executables in TMPDIR, which uses the existing build tmpfs.
// The Go test phase allows up to one hour, bounded by the smaller of the test
// timeout and the configured assurance-attempt deadline.
const baselineGoAssuranceCommand = "git init --quiet; git add --force --all; test -z \"$(gofmt -l .)\"; go vet ./...; go test -timeout 1h ./..."
