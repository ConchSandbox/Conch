package netstack

import (
	"reflect"
	"testing"
)

func TestNormalizeDNSCanonicalizesCNIResult(t *testing.T) {
	in := DNSConfig{
		Nameservers: []string{"10.0.0.53", "10.0.0.53", "10.0.0.54", "1.1.1.1", "8.8.8.8"},
		Domain:      "ignored.example",
		Search:      []string{"one.example", "one.example", "two.example"},
		Options:     []string{"timeout:2", "timeout:2"},
	}

	got, err := NormalizeDNS(in)
	if err != nil {
		t.Fatalf("NormalizeDNS() error = %v", err)
	}
	want := DNSConfig{
		Nameservers: []string{"10.0.0.53", "10.0.0.54", "1.1.1.1"},
		Search:      []string{"one.example", "two.example"},
		Options:     []string{"timeout:2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeDNS() = %#v, want %#v", got, want)
	}
}

func TestNormalizeDNSRejectsUnreachableNameserver(t *testing.T) {
	if _, err := NormalizeDNS(DNSConfig{Nameservers: []string{"127.0.0.53"}}); err == nil {
		t.Fatal("NormalizeDNS() error = nil, want invalid CNI DNS error")
	}
}
