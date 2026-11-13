package controller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestCertificatePredicate_CreateOfTerminatingCertWithFinalizerPasses(t *testing.T) {
	c := predicateCert(nil, []string{annotations.DNSCleanupFinalizer}, true)
	assert.True(t, controller.CertificateAnnotatedPredicate{}.Create(event.CreateEvent{Object: c}))
}

func TestCertificatePredicate_CreateWithoutOptInOrFinalizerSuppressed(t *testing.T) {
	c := predicateCert(nil, []string{annotations.DNSCleanupFinalizer}, false)
	assert.False(t, controller.CertificateAnnotatedPredicate{}.Create(event.CreateEvent{Object: c}))
}

func certWithConditions(ann map[string]string, conds map[string]string) *unstructured.Unstructured {
	c := predicateCert(ann, nil, false)
	var list []any
	for typ, status := range conds {
		list = append(list, map[string]any{"type": typ, "status": status})
	}
	_ = unstructured.SetNestedSlice(c.Object, list, "status", "conditions")
	return c
}

func TestCertificatePredicate_ConditionChangePassesForDNSEnabled(t *testing.T) {
	ann := map[string]string{annotations.Enabled: "true", annotations.DNSEnabled: "true"}
	oldC := certWithConditions(ann, map[string]string{"Ready": "True", "Issuing": "True"})
	newC := certWithConditions(ann, map[string]string{"Ready": "True", "Issuing": "False"})
	assert.True(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_ConditionChangePassesForDNSEnabledViaSecretTemplate(t *testing.T) {
	ann := map[string]string{annotations.Enabled: "true"}
	oldC := certWithConditions(ann, map[string]string{"Ready": "False"})
	newC := certWithConditions(ann, map[string]string{"Ready": "True"})
	for _, c := range []*unstructured.Unstructured{oldC, newC} {
		require.NoError(t, unstructured.SetNestedStringMap(c.Object, map[string]string{annotations.DNSEnabled: "true"}, "spec", "secretTemplate", "annotations"))
	}
	assert.True(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_ConditionChangeSuppressedWithoutDNS(t *testing.T) {
	ann := map[string]string{annotations.Enabled: "true"}
	oldC := certWithConditions(ann, map[string]string{"Ready": "False"})
	newC := certWithConditions(ann, map[string]string{"Ready": "True"})
	assert.False(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_UnchangedStatusSuppressed(t *testing.T) {
	ann := map[string]string{annotations.Enabled: "true", annotations.DNSEnabled: "true"}
	oldC := certWithConditions(ann, map[string]string{"Ready": "True"})
	newC := certWithConditions(ann, map[string]string{"Ready": "True"})
	assert.False(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_IssuingConditionRemovedPasses(t *testing.T) {
	ann := map[string]string{annotations.Enabled: "true", annotations.DNSEnabled: "true"}
	oldC := certWithConditions(ann, map[string]string{"Ready": "True", "Issuing": "True"})
	newC := certWithConditions(ann, map[string]string{"Ready": "True"})
	assert.True(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}

func TestCertificatePredicate_DNSEnabledWithoutOptInSuppressed(t *testing.T) {
	ann := map[string]string{annotations.DNSEnabled: "true"}
	oldC := certWithConditions(ann, map[string]string{"Ready": "False"})
	newC := certWithConditions(ann, map[string]string{"Ready": "True"})
	assert.False(t, controller.CertificateAnnotatedPredicate{}.Update(event.UpdateEvent{ObjectOld: oldC, ObjectNew: newC}))
}
