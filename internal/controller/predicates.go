package controller

import (
	"bytes"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/camilorivera/cert-manager-acm-sync/internal/annotations"
)

// TLSAnnotatedPredicate passes kubernetes.io/tls Secrets to the reconciler.
//
// Create events are passed for all TLS Secrets so that Secrets owned by a
// cert-manager Certificate that carries acm.sync/enabled (rather than
// propagating it via secretTemplate) are picked up on first creation.
//
// Update events:
//   - cert data changes (tls.crt / tls.key) are always passed — needed for
//     renewals of Certificate-annotated certs whose Secret has no annotation.
//   - annotation opt-in toggled on the Secret → reconcile.
//   - for Secrets that are themselves annotated, changes where only
//     acm.sync/* keys changed (our own write-back) are suppressed to prevent
//     an infinite reconcile loop.
//   - annotation-only changes on unannotated Secrets are suppressed.
type TLSAnnotatedPredicate struct {
	predicate.Funcs
}

var _ predicate.Predicate = TLSAnnotatedPredicate{}

func (TLSAnnotatedPredicate) Create(e event.CreateEvent) bool {
	s, ok := e.Object.(*corev1.Secret)
	return ok && s.Type == corev1.SecretTypeTLS
}

func (TLSAnnotatedPredicate) Update(e event.UpdateEvent) bool {
	newS, ok := e.ObjectNew.(*corev1.Secret)
	if !ok || newS.Type != corev1.SecretTypeTLS {
		return false
	}
	oldS, ok := e.ObjectOld.(*corev1.Secret)
	if !ok {
		return true
	}

	// Cert data changed → always pass through (covers renewals of
	// Certificate-annotated certs where the Secret has no annotation).
	if !bytes.Equal(oldS.Data["tls.crt"], newS.Data["tls.crt"]) ||
		!bytes.Equal(oldS.Data["tls.key"], newS.Data["tls.key"]) {
		return true
	}

	// Annotation opt-in was toggled on the Secret itself → reconcile.
	if annotations.IsEnabled(oldS.Annotations) != annotations.IsEnabled(newS.Annotations) {
		return true
	}

	// For Secret-annotated resources, suppress our own acm.sync/* write-backs.
	if annotations.IsEnabled(newS.Annotations) {
		oldFiltered := annotations.StripACMAnnotations(oldS.Annotations)
		newFiltered := annotations.StripACMAnnotations(newS.Annotations)
		return !reflect.DeepEqual(oldFiltered, newFiltered)
	}

	// Annotation-only change on an unannotated Secret → suppress.
	return false
}

func (TLSAnnotatedPredicate) Delete(_ event.DeleteEvent) bool   { return false }
func (TLSAnnotatedPredicate) Generic(_ event.GenericEvent) bool { return false }

// CertificateAnnotatedPredicate filters cert-manager Certificate events for
// the secondary watch. It passes Creates/Updates only when the Certificate
// carries acm.sync/enabled="true", and suppresses updates where solely
// acm.sync/* annotations changed (our own ARN write-back) to prevent loops.
type CertificateAnnotatedPredicate struct {
	predicate.Funcs
}

var _ predicate.Predicate = CertificateAnnotatedPredicate{}

func (CertificateAnnotatedPredicate) Create(e event.CreateEvent) bool {
	return annotations.IsEnabled(e.Object.GetAnnotations())
}

func (CertificateAnnotatedPredicate) Update(e event.UpdateEvent) bool {
	if !annotations.IsEnabled(e.ObjectNew.GetAnnotations()) {
		return false
	}
	oldFiltered := annotations.StripACMAnnotations(e.ObjectOld.GetAnnotations())
	newFiltered := annotations.StripACMAnnotations(e.ObjectNew.GetAnnotations())
	return !reflect.DeepEqual(oldFiltered, newFiltered)
}

func (CertificateAnnotatedPredicate) Delete(_ event.DeleteEvent) bool   { return false }
func (CertificateAnnotatedPredicate) Generic(_ event.GenericEvent) bool { return false }
