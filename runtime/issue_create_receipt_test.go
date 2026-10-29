package runtime

import "testing"

// TestIssueCreateReceiptNamesExactlyOneIssue: the receipt a lost reply is
// reconciled against must name the repository and issue number and nothing
// belonging to another action kind.
func TestIssueCreateReceiptNamesExactlyOneIssue(t *testing.T) {
	valid := ArtifactActionReceipt{
		OperationID: artifactReceiptTestID, RequestDigest: artifactReceiptTestDigest,
		Action: ControlIssueCreate, ControllerBinding: "binding", Repository: "acme/repo", SourceIssue: 7,
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("a complete issue-create receipt: %v", err)
	}
	for name, edit := range map[string]func(*ArtifactActionReceipt){
		"no repository":     func(r *ArtifactActionReceipt) { r.Repository = "" },
		"no issue number":   func(r *ArtifactActionReceipt) { r.SourceIssue = 0 },
		"carries a product": func(r *ArtifactActionReceipt) { r.ProductID = "widget" },
	} {
		r := valid
		edit(&r)
		if r.validate() == nil {
			t.Errorf("%s: a malformed issue-create receipt was accepted", name)
		}
	}
	product := validProductReceipt()
	product.SourceIssue = 7
	if product.validate() == nil {
		t.Error("a product receipt carrying an issue number was accepted")
	}
}
