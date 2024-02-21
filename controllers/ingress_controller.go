/*
Copyright 2023 Django Cass.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw"
	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/providers/ingressnginx"
	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/providers/kong"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	"slices"
)

// IngressReconciler reconciles an Ingress object
type IngressReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	WatchedClasses []string
	BetaResources  bool
}

//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/finalizers,verbs=update
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Ingress object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name)

	logger.Info("reconciling ingress")
	ing := &netv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ing); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if ing.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// check if the ingress is one we should be watching
	if len(r.WatchedClasses) > 0 && ing.Spec.IngressClassName != nil && !slices.Contains(r.WatchedClasses, *ing.Spec.IngressClassName) {
		logger.Info("skipping Ingress with non-matching class", "expected", r.WatchedClasses, "actual", *ing.Spec.IngressClassName)
		return ctrl.Result{}, nil
	}

	if err := r.reconcileGatewayResources(ctx, ing); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		Owns(&gatewayv1beta1.HTTPRoute{}).
		Complete(r)
}

func (r *IngressReconciler) reconcileGatewayResources(ctx context.Context, ing *netv1.Ingress) error {
	logger := log.FromContext(ctx)
	logger.Info("checking providers")
	providerByName, err := r.constructProvider(&i2gw.ProviderConf{Client: r.Client}, []string{ingressnginx.Name, kong.Name})
	if err != nil {
		return err
	}
	ns := gatewayv1beta1.Namespace(ing.Annotations["networking.k8s.io/gateway-namespace"])

	for name, provider := range providerByName {
		logger.Info("converting resources for provider", "provider", name)
		gatewayResources, conversionErrors := provider.ToGatewayAPI(i2gw.InputResources{Ingresses: []netv1.Ingress{*ing}})
		for k, v := range gatewayResources.Gateways {
			logger.Info("generated gateway", "name", k.String(), "resource", v)
		}
		for k, v := range gatewayResources.HTTPRoutes {
			logger.Info("generated httproute", "name", k.String(), "resource", v)
			// update the parent ref namespace
			for i := range v.Spec.ParentRefs {
				if v.Spec.ParentRefs[i].Namespace == nil || *v.Spec.ParentRefs[i].Namespace == "" {
					logger.Info("overwriting empty parentRef namespace", "namespace", ns)
					v.Spec.ParentRefs[i].Namespace = &ns
				}
			}
			if err := r.reconcileHttpRoute(ctx, ing, &v); err != nil {
				return err
			}
		}
		logger.Info("converted ingress to gateway", "conversionErrors", conversionErrors)
	}

	return nil
}

func (r *IngressReconciler) reconcileHttpRoute(ctx context.Context, ing *netv1.Ingress, cr *gatewayv1beta1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	found := &gatewayv1beta1.HTTPRoute{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}, found); err != nil {
		if errors.IsNotFound(err) {
			_ = controllerutil.SetControllerReference(ing, &cr.ObjectMeta, r.Scheme)
			logger.Info("creating HTTPRoute")
			if err := r.Create(ctx, cr); err != nil {
				logger.Error(err, "failed to create HTTPRoute")
				return err
			}
			r.Recorder.Eventf(ing, corev1.EventTypeNormal, ReasonCreated, "Created HTTPRoute %s", cr.Name)
			return nil
		}
		return err
	}
	_ = controllerutil.SetControllerReference(ing, &cr.ObjectMeta, r.Scheme)
	// reconcile by forcibly overwriting
	// any changes
	var requiresUpdate bool
	if !reflect.DeepEqual(cr.Annotations, found.Annotations) {
		logger.Info("unexpected HTTPRoute annotations")
		requiresUpdate = true
	}
	if !reflect.DeepEqual(cr.Labels, found.Labels) {
		logger.Info("unexpected HTTPRoute labels")
		requiresUpdate = true
	}
	if !reflect.DeepEqual(cr.Spec, found.Spec) {
		logger.Info("unexpected HTTPRoute spec")
		requiresUpdate = true
	}
	if requiresUpdate {
		r.Recorder.Eventf(ing, corev1.EventTypeWarning, ReasonRequiresUpdate, "HTTPRoute %s requires reconciliation", cr.Name)
		return r.SafeUpdate(ctx, found, cr)
	}
	return nil
}

func (*IngressReconciler) constructProvider(conf *i2gw.ProviderConf, providers []string) (map[i2gw.ProviderName]i2gw.Provider, error) {
	providerByName := make(map[i2gw.ProviderName]i2gw.Provider, len(i2gw.ProviderConstructorByName))

	for _, requestedProvider := range providers {
		requestedProviderName := i2gw.ProviderName(requestedProvider)
		newProviderFunc, ok := i2gw.ProviderConstructorByName[requestedProviderName]
		if !ok {
			return nil, fmt.Errorf("%s is not a supported provider", requestedProvider)
		}
		providerByName[requestedProviderName] = newProviderFunc(conf)
	}

	return providerByName, nil
}

// SafeUpdate calls Update with hacks required to ensure that
// the update is applied correctly.
//
// https://github.com/argoproj/argo-cd/issues/3657
func (r *IngressReconciler) SafeUpdate(ctx context.Context, old, new client.Object, option ...client.UpdateOption) error {
	new.SetResourceVersion(old.GetResourceVersion())
	return r.Update(ctx, new, option...)
}
