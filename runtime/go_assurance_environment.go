package runtime

// The detached assurance checkout contains only the bound candidate tree.
// Populate the masked, ephemeral Git directory so repository-aware checks can
// enumerate that tree without receiving host Git configuration or history.
// Tests create executables in TMPDIR, which uses the existing build tmpfs.
// Let the suite exceed the observed 15-minute Go timeout within the runtime's
// existing 25-minute assurance-attempt ceiling.
const baselineGoAssuranceCommand = "git init --quiet; git add --force --all; test -z \"$(gofmt -l .)\"; go vet ./...; go test -timeout 1h ./..."
