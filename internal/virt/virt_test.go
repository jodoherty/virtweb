package virt

import "testing"

func TestDomainName(t *testing.T) {
	cases := []struct {
		xml  string
		want string
	}{
		{"<domain type='kvm'><name>myvm</name><vcpu>2</vcpu></domain>", "myvm"},
		// an interface <name> later in the document must not win
		{"<domain type='kvm'><name>myvm</name><devices><interface><name>eth0</name></interface></devices></domain>", "myvm"},
		{"<domain type='qemu'>\n  <name> spaced </name>\n</domain>", "spaced"},
		{"<domain type='kvm'><vcpu>2</vcpu></domain>", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := DomainName(tc.xml); got != tc.want {
			t.Errorf("DomainName(%q) = %q, want %q", tc.xml, got, tc.want)
		}
	}
}
