// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package enterprisesearch

import (
	"context"
	"crypto/sha256"
	"fmt"

	"go.elastic.co/apm"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	entv1beta1 "github.com/elastic/cloud-on-k8s/pkg/apis/enterprisesearch/v1beta1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/association"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/annotation"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/certificates"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/defaults"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/driver"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/events"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/operator"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/tracing"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/watches"
	entName "github.com/elastic/cloud-on-k8s/pkg/controller/enterprisesearch/name"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
)

const (
	controllerName = "enterprisesearch-controller"
)

var (
	log = logf.Log.WithName(controllerName)
)

// Add creates a new EnterpriseSearch Controller and adds it to the Manager with default RBAC.
//The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, params operator.Parameters) error {
	reconciler := newReconciler(mgr, params)
	c, err := common.NewController(mgr, controllerName, reconciler, params)
	if err != nil {
		return err
	}
	return addWatches(c, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, params operator.Parameters) *ReconcileEnterpriseSearch {
	client := k8s.WrapClient(mgr.GetClient())
	return &ReconcileEnterpriseSearch{
		Client:         client,
		recorder:       mgr.GetEventRecorderFor(controllerName),
		dynamicWatches: watches.NewDynamicWatches(),
		Parameters:     params,
	}
}

func podsToReconcilerequest(object handler.MapObject) []reconcile.Request {
	labels := object.Meta.GetLabels()
	entName, isSet := labels[EnterpriseSearchNameLabelName]
	if !isSet {
		return nil
	}
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: object.Meta.GetNamespace(),
				Name:      entName,
			},
		},
	}
}

func addWatches(c controller.Controller, r *ReconcileEnterpriseSearch) error {
	// Watch for changes to EnterpriseSearch
	err := c.Watch(&source.Kind{Type: &entv1beta1.EnterpriseSearch{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch Deployments
	if err := c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &entv1beta1.EnterpriseSearch{},
	}); err != nil {
		return err
	}

	// Watch Pods to be notified about Pods going up/down during version upgrades.
	// Unfortunately watching deployment only is not enough since we may miss a Pod deletion event.
	if err := c.Watch(&source.Kind{Type: &corev1.Pod{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(podsToReconcilerequest),
		}); err != nil {
		return err
	}

	// Watch services
	if err := c.Watch(&source.Kind{Type: &corev1.Service{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &entv1beta1.EnterpriseSearch{},
	}); err != nil {
		return err
	}

	// Watch secrets
	if err := c.Watch(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &entv1beta1.EnterpriseSearch{},
	}); err != nil {
		return err
	}

	// Dynamically watch referenced secrets to connect to Elasticsearch
	if err := c.Watch(&source.Kind{Type: &corev1.Secret{}}, r.dynamicWatches.Secrets); err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileEnterpriseSearch{}

// ReconcileEnterpriseSearch reconciles an ApmServer object
type ReconcileEnterpriseSearch struct {
	k8s.Client
	recorder       record.EventRecorder
	dynamicWatches watches.DynamicWatches
	operator.Parameters
	// iteration is the number of times this controller has run its Reconcile method
	iteration uint64
}

func (r *ReconcileEnterpriseSearch) K8sClient() k8s.Client {
	return r.Client
}

func (r *ReconcileEnterpriseSearch) DynamicWatches() watches.DynamicWatches {
	return r.dynamicWatches
}

func (r *ReconcileEnterpriseSearch) Recorder() record.EventRecorder {
	return r.recorder
}

var _ driver.Interface = &ReconcileEnterpriseSearch{}

// Reconcile reads that state of the cluster for an EnterpriseSearch object and makes changes based on the state read
// and what is in the EnterpriseSearch.Spec.
func (r *ReconcileEnterpriseSearch) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	defer common.LogReconciliationRun(log, request, "ent_name", &r.iteration)()
	tx, ctx := tracing.NewTransaction(r.Tracer, request.NamespacedName, "enterprisesearch")
	defer tracing.EndTransaction(tx)

	var ent entv1beta1.EnterpriseSearch
	if err := association.FetchWithAssociation(ctx, r.Client, request, &ent); err != nil {
		if apierrors.IsNotFound(err) {
			r.onDelete(types.NamespacedName{
				Namespace: request.Namespace,
				Name:      request.Name,
			})
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, tracing.CaptureError(ctx, err)
	}

	if common.IsUnmanaged(&ent) {
		log.Info("Object is currently not managed by this controller. Skipping reconciliation", "namespace", ent.Namespace, "ent_name", ent.Name)
		return reconcile.Result{}, nil
	}

	if compatible, err := r.isCompatible(ctx, &ent); err != nil || !compatible {
		return reconcile.Result{}, tracing.CaptureError(ctx, err)
	}

	if ent.IsMarkedForDeletion() {
		// Enterprise Search will be deleted, clean up resources
		r.onDelete(k8s.ExtractNamespacedName(&ent))
		return reconcile.Result{}, nil
	}

	if err := annotation.UpdateControllerVersion(ctx, r.Client, &ent, r.OperatorInfo.BuildInfo.Version); err != nil {
		return reconcile.Result{}, tracing.CaptureError(ctx, err)
	}

	if !association.IsConfiguredIfSet(&ent, r.recorder) {
		return reconcile.Result{}, nil
	}

	return r.doReconcile(ctx, request, ent)
}

func (r *ReconcileEnterpriseSearch) onDelete(obj types.NamespacedName) {
	// Clean up watches
	r.dynamicWatches.Secrets.RemoveHandlerForKey(configRefWatchName(obj))
}

func (r *ReconcileEnterpriseSearch) isCompatible(ctx context.Context, ent *entv1beta1.EnterpriseSearch) (bool, error) {
	selector := map[string]string{EnterpriseSearchNameLabelName: ent.Name}
	compat, err := annotation.ReconcileCompatibility(ctx, r.Client, ent, selector, r.OperatorInfo.BuildInfo.Version)
	if err != nil {
		k8s.EmitErrorEvent(r.recorder, err, ent, events.EventCompatCheckError, "Error during compatibility check: %v", err)
	}
	return compat, err
}

func (r *ReconcileEnterpriseSearch) doReconcile(ctx context.Context, request reconcile.Request, ent entv1beta1.EnterpriseSearch) (reconcile.Result, error) {
	// Run validation in case the webhook is disabled
	if err := r.validate(ctx, &ent); err != nil {
		return reconcile.Result{}, err
	}

	state := NewState(request, &ent)

	svc, err := common.ReconcileService(ctx, r.Client, NewService(ent), &ent)
	if err != nil {
		return reconcile.Result{}, err
	}

	_, results := certificates.Reconciler{
		K8sClient:             r.K8sClient(),
		DynamicWatches:        r.DynamicWatches(),
		Object:                &ent,
		TLSOptions:            ent.Spec.HTTP.TLS,
		Namer:                 entName.EntNamer,
		Labels:                Labels(ent.Name),
		Services:              []corev1.Service{*svc},
		CACertRotation:        r.CACertRotation,
		CertRotation:          r.CertRotation,
		GarbageCollectSecrets: true,
	}.ReconcileCAAndHTTPCerts(ctx)
	if results.HasError() {
		res, err := results.Aggregate()
		k8s.EmitErrorEvent(r.recorder, err, &ent, events.EventReconciliationError, "Certificate reconciliation error: %v", err)
		return res, err
	}

	if err := watchEsTLSCertsSecret(ent, r.DynamicWatches()); err != nil {
		return reconcile.Result{}, err
	}

	configSecret, err := ReconcileConfig(r, ent)
	if err != nil {
		return reconcile.Result{}, err
	}

	// toggle read-only mode for Enterprise Search version upgrades
	upgrade := VersionUpgrade{k8sClient: r.K8sClient(), recorder: r.Recorder(), ent: ent, dialer: r.Dialer}
	if err := upgrade.Handle(ctx); err != nil {
		return reconcile.Result{}, err
	}

	// build a hash of various inputs to rotate Pods on any change
	configHash, err := buildConfigHash(r.K8sClient(), ent, configSecret)
	if err != nil {
		return reconcile.Result{}, err
	}

	state, err = r.reconcileDeployment(ctx, state, ent, configHash)
	if err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("Conflict while updating status")
			return reconcile.Result{Requeue: true}, nil
		}
		k8s.EmitErrorEvent(r.recorder, err, &ent, events.EventReconciliationError, "Deployment reconciliation error: %v", err)
		return state.Result, tracing.CaptureError(ctx, err)
	}

	state.UpdateEnterpriseSearchExternalService(*svc)

	// TODO: update status

	res, err := results.WithError(err).Aggregate()
	k8s.EmitErrorEvent(r.recorder, err, &ent, events.EventReconciliationError, "Reconciliation error: %v", err)
	return res, nil
}

func (r *ReconcileEnterpriseSearch) validate(ctx context.Context, ent *entv1beta1.EnterpriseSearch) error {
	span, vctx := apm.StartSpan(ctx, "validate", tracing.SpanTypeApp)
	defer span.End()

	if err := ent.ValidateCreate(); err != nil {
		log.Error(err, "Validation failed")
		k8s.EmitErrorEvent(r.recorder, err, ent, events.EventReasonValidation, err.Error())
		return tracing.CaptureError(vctx, err)
	}

	return nil
}

func NewService(ent entv1beta1.EnterpriseSearch) *corev1.Service {
	svc := corev1.Service{
		ObjectMeta: ent.Spec.HTTP.Service.ObjectMeta,
		Spec:       ent.Spec.HTTP.Service.Spec,
	}

	svc.ObjectMeta.Namespace = ent.Namespace
	svc.ObjectMeta.Name = entName.HTTPService(ent.Name)

	labels := Labels(ent.Name)
	ports := []corev1.ServicePort{
		{
			Name:     ent.Spec.HTTP.Protocol(),
			Protocol: corev1.ProtocolTCP,
			Port:     HTTPPort,
		},
	}

	return defaults.SetServiceDefaults(&svc, labels, labels, ports)
}

func buildConfigHash(c k8s.Client, ent entv1beta1.EnterpriseSearch, configSecret corev1.Secret) (string, error) {
	// build a hash of various settings to rotate the Pod on any change
	configHash := sha256.New224()

	// - in the Enterprise Search configuration file content
	_, _ = configHash.Write(configSecret.Data[ConfigFilename])

	// - in the Enterprise Search TLS certificates
	var tlsCertSecret corev1.Secret
	tlsSecretKey := types.NamespacedName{Namespace: ent.Namespace, Name: certificates.InternalCertsSecretName(entName.EntNamer, ent.Name)}
	if err := c.Get(tlsSecretKey, &tlsCertSecret); err != nil {
		return "", err
	}
	if certPem, ok := tlsCertSecret.Data[certificates.CertFileName]; ok {
		_, _ = configHash.Write(certPem)
	}

	// - in the Elasticsearch TLS certificates
	if ent.AssociationConf().CAIsConfigured() {
		var esPublicCASecret corev1.Secret
		key := types.NamespacedName{Namespace: ent.Namespace, Name: ent.AssociationConf().GetCASecretName()}
		if err := c.Get(key, &esPublicCASecret); err != nil {
			return "", err
		}
		if certPem, ok := esPublicCASecret.Data[certificates.CertFileName]; ok {
			_, _ = configHash.Write(certPem)
		}
	}

	return fmt.Sprintf("%x", configHash.Sum(nil)), nil
}

func tlsSecretWatchName(ent entv1beta1.EnterpriseSearch) string {
	return fmt.Sprintf("%s-%s-es-auth-secret", ent.Namespace, ent.Name)
}

// watchEsTLSCertsSecret sets up a dynamic watch for the Secret containing the associated Elasticsearch TLS CA certs.
func watchEsTLSCertsSecret(ent entv1beta1.EnterpriseSearch, watched watches.DynamicWatches) error {
	if !ent.AssociationConf().CAIsConfigured() {
		return nil
	}
	return watched.Secrets.AddHandler(watches.NamedWatch{
		Name:    tlsSecretWatchName(ent),
		Watched: []types.NamespacedName{{Namespace: ent.Namespace, Name: ent.AssociationConf().GetCASecretName()}},
		Watcher: k8s.ExtractNamespacedName(&ent),
	})
}
