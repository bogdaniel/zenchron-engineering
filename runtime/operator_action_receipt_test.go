package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOperatorActionReceiptIsObservationalDurableAndClosed(t *testing.T) {
	dir, store := openJournal(t)
	id := "01234567-89ab-4cde-8fab-0123456789ab"
	receipt := OperatorActionReceipt{OperationID: id, RequestDigest: strings.Repeat("a", 64), Action: ControlResume,
		ControllerBinding: "binding", SourceRunID: "r", RunID: "r"}
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{EventPlanOperatorActionReceipt, EventRunOperatorActionReceipt} {
		if kind == EventPlanOperatorActionReceipt && eventPayloads[kind](payload) == nil {
			t.Fatal("run receipt admitted into plan stream")
		}
	}
	if err := eventPayloads[EventRunOperatorActionReceipt]([]byte(strings.TrimSuffix(string(payload), "}") + `,"note":"PRIVATE"}`)); err == nil {
		t.Fatal("receipt admitted annotation")
	}
	event, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "operator-action-" + id, OperationID: id,
		RunID: "r", Type: EventRunOperatorActionReceipt, OccurredAt: time.Unix(100, 0).UTC(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if event.StateBefore != event.StateAfter {
		t.Fatal("delivery observation changed runtime state")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.OperatorActionReceipt(id)
	if err != nil || !reflect.DeepEqual(got, &receipt) {
		t.Fatalf("receipt not durable: %+v %v", got, err)
	}
	if _, err := reader.store.db.Exec("DELETE FROM events"); err == nil {
		t.Fatal("receipt reader became writable")
	}
	db := rawJournalDB(t, dir)
	if _, err := db.Exec("UPDATE events SET document=replace(document,'binding','tampered') WHERE id=?", event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.OperatorActionReceipt(id); err == nil {
		t.Fatal("tampered receipt admitted")
	}
}
