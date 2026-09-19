package protocol

import (
	"strings"
	"testing"
)

func TestPermissionMenuValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		menu  *PermissionMenu
		valid bool
	}{
		{"valid", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: "Ask for approval"}}}, true},
		{"unicode boundaries", &PermissionMenu{Options: []PermissionOption{{ID: strings.Repeat("x", 256), Label: strings.Repeat("é", 120), Description: strings.Repeat("é", 500)}}}, true},
		{"nil", nil, false},
		{"empty", &PermissionMenu{}, false},
		{"too many", &PermissionMenu{Options: make([]PermissionOption, 51)}, false},
		{"blank id", &PermissionMenu{Options: []PermissionOption{{ID: " ", Label: "Label"}}}, false},
		{"long id", &PermissionMenu{Options: []PermissionOption{{ID: strings.Repeat("x", 257), Label: "Label"}}}, false},
		{"control id", &PermissionMenu{Options: []PermissionOption{{ID: "foo\nbar", Label: "Label"}}}, false},
		{"invalid id utf8", &PermissionMenu{Options: []PermissionOption{{ID: "\xff", Label: "Label"}}}, false},
		{"blank label", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: "\t"}}}, false},
		{"long label", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: strings.Repeat("é", 121)}}}, false},
		{"invalid label utf8", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: "\xff"}}}, false},
		{"long description", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: "Label", Description: strings.Repeat("é", 501)}}}, false},
		{"invalid description utf8", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: "Label", Description: "\xff"}}}, false},
		{"duplicate", &PermissionMenu{Options: []PermissionOption{{ID: "default", Label: "First"}, {ID: "default", Label: "Second"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.menu.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate()=%v, valid=%v", err, tc.valid)
			}
		})
	}
}
