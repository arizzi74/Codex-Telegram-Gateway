package protocol

import (
	"strings"
	"testing"
)

func TestWorkerEnrollmentCodeNormalization(t *testing.T) {
	for _, input := range []string{"ABCD2345WXYZ", "abcd2345wxyz", "AbCd2345wXyZ"} {
		got, err := NormalizeWorkerEnrollmentCode(input)
		if err != nil || got != "ABCD2345WXYZ" {
			t.Fatalf("canonical code: got %q, error %v", got, err)
		}
	}
	for _, input := range []string{"", "ABCD2345WXY", "ABCD2345WXYZZ", "ABCD2345WXY0", "ABCD2345WXY1", "ABCD2345WXYI", "ABCD2345WXYO", " ABCD2345WXYZ", "ABCD2345WXY\n", "ABCD2345WXK", "ABCD2345WXY%", "ABCD2345WXY/"} {
		if _, err := NormalizeWorkerEnrollmentCode(input); err == nil {
			t.Errorf("accepted invalid code %q", input)
		}
	}
}

func TestWorkerEnrollmentNames(t *testing.T) {
	for _, name := range []string{"Build worker", "日本のワーカー", "  Worker  ", strings.Repeat("é", 160)} {
		if err := ValidateWorkerEnrollmentName(name); err != nil {
			t.Errorf("rejected valid name: %v", err)
		}
	}
	for _, name := range []string{"", "  ", strings.Repeat("é", 161), "worker\nother", "worker\r", "worker\t", "worker\x00", "worker\x1b", "worker\u2028other", "worker\u2029other", "invalid\xff"} {
		if err := ValidateWorkerEnrollmentName(name); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
}

func TestWorkerEnrollmentAccessDefaultsRestricted(t *testing.T) {
	for _, input := range []string{"", "restricted", "full"} {
		got, err := NormalizeWorkerEnrollmentServiceAccess(input)
		if err != nil || (input == "" && got != "restricted") || (input != "" && got != input) {
			t.Fatalf("invalid access normalization for %q: %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"root", "Full", "full ", " unrestricted", "../full"} {
		if _, err := NormalizeWorkerEnrollmentServiceAccess(input); err == nil {
			t.Errorf("accepted invalid service access %q", input)
		}
	}
}
