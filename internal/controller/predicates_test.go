package controller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
	"github.com/camilorivera/cert-manager-acm-sync/internal/controller"
)

func predicateSecret(ann map[string]string, finalizers []string, deleting bool) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", Annotations: ann, Finalizers: finalizers},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k")},
	}
	if deleting {
		now := metav1.Now()
		s.DeletionTimestamp = &now
	}
	return s
}

func predicateCert(ann map[string]string, finalizers []string, deleting bool) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetName("c")
	c.SetNamespace("default")
	c.SetAnnotations(ann)
	c.SetFinalizers(finalizers)
	if deleting {
		now := metav1.Now()
		c.SetDeletionTimestamp(&now)
	}
	return c
}

func TestTLSPredicate_DNSRecordsOnlyChangeSuppressed(t *testing.T) {
	oldS := predicateSecret(map[string]string{annotations.Enabled: "true"}, nil, false)
	newS := predicateSecret(map[string]string{annotations.Enabled: "true", annotations.DNSRecords: "a.example.com"}, nil, false)
	assert.False(t, controller.TLSAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: newS}))
}

func TestTLSPredicate_UserDNSAnnotationChangePasses(t *testing.T) {
	oldS := predicateSecret(map[string]string{annotations.Enabled: "true"}, nil, false)
	newS := predicateSecret(map[string]string{annotations.Enabled: "true", annotations.DNSTarget: "x.example.com"}, nil, false)
	assert.True(t, controller.TLSAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: newS}))
}

func TestTLSPredicate_DeletionWithFinalizerPasses(t *testing.T) {
	fin := []string{annotations.DNSCleanupFinalizer}
	oldS := predicateSecret(map[string]string{annotations.Enabled: "true"}, fin, false)
	newS := predicateSecret(map[string]string{annotations.Enabled: "true"}, fin, true)
	assert.True(t, controller.TLSAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: newS}))
}

func TestTLSPredicate_DeletionWithoutFinalizerSuppressed(t *testing.T) {
	oldS := predicateSecret(map[string]string{annotations.Enabled: "true"}, nil, false)
	newS := predicateSecret(map[string]string{annotations.Enabled: "true"}, nil, true)
	assert.False(t, controller.TLSAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: newS}))
}

func TestCertificatePredicate_DNSRecordsOnlyChangeSuppressed(t *testing.T) {
	oldC := predicateCert(map[string]string{annotations.Enabled: "true"}, nil, false)
	newC := predicateCert(map[string]string{annotations.Enabled: "true", annotations.DNSRecords: "a.example.com"}, nil, false)
	assert.False(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_UserDNSAnnotationChangePasses(t *testing.T) {
	oldC := predicateCert(map[string]string{annotations.Enabled: "true"}, nil, false)
	newC := predicateCert(map[string]string{annotations.Enabled: "true", annotations.DNSEnabled: "true"}, nil, false)
	assert.True(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_DeletionWithFinalizerPasses(t *testing.T) {
	fin := []string{annotations.DNSCleanupFinalizer}
	oldC := predicateCert(map[string]string{annotations.Enabled: "true"}, fin, false)
	newC := predicateCert(map[string]string{annotations.Enabled: "true"}, fin, true)
	assert.True(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_DeletionWithoutFinalizerSuppressed(t *testing.T) {
	oldC := predicateCert(map[string]string{annotations.Enabled: "true"}, nil, false)
	newC := predicateCert(map[string]string{annotations.Enabled: "true"}, nil, true)
	assert.False(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}
