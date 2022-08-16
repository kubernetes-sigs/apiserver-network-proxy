package tests

import (
	"flag"
	"testing"

	"k8s.io/klog/v2"
)

func TestMain(m *testing.M) {
	fs := flag.NewFlagSet("mock-flags", flag.PanicOnError)
	klog.InitFlags(fs)
	fs.Set("v", "9") // Set klog to max verbosity.

	m.Run()
}
