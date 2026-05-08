package controller

import (
	"bytes"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
)

// TLSAnnotatedPredicate passes only Secrets that are:
//   - type=kubernetes.io/tls
//   - annotated with acm.sync/enabled="true"
//
// On Update events it additionally suppresses events caused solely by the
// controller writing its own acm.sync/* annotations back onto the Secret,
// preventing an infinite reconcile loop.
type TLSAnnotatedPredicate struct {
	predicate.Funcs
}

var _ predicate.Predicate = TLSAnnotatedPredicate{}

func isEligible(obj client.Object) bool {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return false
	}
	if s.Type != corev1.SecretTypeTLS {
		return false
	}
	return annotations.IsEnabled(s.Annotations)
}

func (TLSAnnotatedPredicate) Create(e event.CreateEvent) bool {
	return isEligible(e.Object)
}

func (TLSAnnotatedPredicate) Update(e event.UpdateEvent) bool {
	if !isEligible(e.ObjectNew) {
		return false
	}

	oldS, okOld := e.ObjectOld.(*corev1.Secret)
	newS, okNew := e.ObjectNew.(*corev1.Secret)
	if !okOld || !okNew {
		return true
	}

	// Cert data changed → always reconcile
	if !bytes.Equal(oldS.Data["tls.crt"], newS.Data["tls.crt"]) ||
		!bytes.Equal(oldS.Data["tls.key"], newS.Data["tls.key"]) {
		return true
	}

	// Annotation opt-in was just added → reconcile
	if !annotations.IsEnabled(oldS.Annotations) && annotations.IsEnabled(newS.Annotations) {
		return true
	}

	// Only acm.sync/* annotations changed (our own write-back) → suppress
	oldFiltered := annotations.StripACMAnnotations(oldS.Annotations)
	newFiltered := annotations.StripACMAnnotations(newS.Annotations)
	if reflect.DeepEqual(oldFiltered, newFiltered) {
		return false
	}

	return true
}

func (TLSAnnotatedPredicate) Delete(_ event.DeleteEvent) bool  { return false }
func (TLSAnnotatedPredicate) Generic(_ event.GenericEvent) bool { return false }
