package document

import "testing"

func TestVLMSettingsConfigured(t *testing.T) {
	if (VLMSettings{}).Configured() {
		t.Fatalf("unconfigured endpoint reported as configured")
	}
	if (VLMSettings{BaseURL: "   "}).Configured() {
		t.Fatalf("blank endpoint reported as configured")
	}
	if !(VLMSettings{BaseURL: "http://vlm.test/v1"}).Configured() {
		t.Fatalf("configured endpoint reported as unconfigured")
	}
}
