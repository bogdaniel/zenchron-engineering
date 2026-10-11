package runtime

// The detached assurance checkout contains only the bound candidate tree.
// Populate the masked, ephemeral Git directory so repository-aware checks can
// enumerate that tree without receiving host Git configuration or history.
// Tests create executables in TMPDIR, which uses the existing build tmpfs.
// Go's test runner gets one hour; the runtime's authorized attempt and wall
// deadlines remain independent outer limits.
const baselineGoAssuranceCommand = "git init --quiet; git add --force --all; test -z \"$(gofmt -l .)\"; go vet ./...; go test -timeout 1h ./..."
