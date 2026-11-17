package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (r *WorkloadReconciler) eventNormal(object runtime.Object, reason, message string, args ...interface{}) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, corev1.EventTypeNormal, reason, message, args...)
	}
}

func (r *WorkloadReconciler) eventWarning(object runtime.Object, reason, message string, args ...interface{}) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, corev1.EventTypeWarning, reason, message, args...)
	}
}
