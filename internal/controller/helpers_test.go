package controller_test

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/config"
)

type noopRecorder struct{}

func (noopRecorder) Eventf(runtime.Object, runtime.Object, string, string, string, string, ...any) {}

// configController allows several managers with the same controller name in one process.
func configController() config.Controller { return config.Controller{SkipNameValidation: ptr.To(true)} }
