package state

import "testing"

func TestDecodeReembedNotification(t *testing.T) {
	const id = "46d8dbe0-db70-4c09-ad51-4a3e70d8a3ca"
	namespace, jobID, err := DecodeReembedNotification([]byte(`{"before":null,"after":{"namespace":"alpha","job_id":"` + id + `"},"op":"c"}`))
	if err != nil || namespace != "alpha" || jobID != id {
		t.Fatalf("DecodeReembedNotification() = %q, %q, %v", namespace, jobID, err)
	}
	if _, _, err := DecodeReembedNotification([]byte("null")); !IgnorableCDCChange(err) {
		t.Fatalf("tombstone error = %v, want ignorable CDC change", err)
	}
	if _, _, err := DecodeReembedNotification([]byte(`{"after":{"namespace":"alpha","job_id":"invalid"},"op":"c"}`)); err == nil {
		t.Fatal("accepted an invalid job identifier")
	}
}
