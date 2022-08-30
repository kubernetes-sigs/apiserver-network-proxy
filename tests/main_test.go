package tests

import (
	"flag"
	"testing"

	"k8s.io/klog/v2"
)

func TestMain(m *testing.M) {
	fs := flag.NewFlagSet("mock-flags", flag.PanicOnError)
	klog.InitFlags(fs)
	fs.Set("v", "1") // Set klog verbosity.

	m.Run()
}
