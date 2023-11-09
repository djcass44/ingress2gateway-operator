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
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// IngressReconciler reconciles an Ingress object
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

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

	if err := r.reconcileHttpRoute(ctx, ing); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Complete(r)
}

func (r *IngressReconciler) reconcileHttpRoute(ctx context.Context, ing *netv1.Ingress) error {
	logger := log.FromContext(ctx)
	logger.Info("checking providers")
	providerByName, err := r.constructProvider(&i2gw.ProviderConf{Client: r.Client}, []string{ingressnginx.Name, kong.Name})
	if err != nil {
		return err
	}

	for name, provider := range providerByName {
		logger.Info("converting resources for provider", "provider", name)
		gatewayResources, conversionErrors := provider.ToGatewayAPI(i2gw.InputResources{Ingresses: []netv1.Ingress{*ing}})
		for k, v := range gatewayResources.Gateways {
			logger.Info("generated gateway", "name", k.String(), "resource", v)
		}
		for k, v := range gatewayResources.HTTPRoutes {
			logger.Info("generated httproute", "name", k.String(), "resource", v)
		}
		logger.Info("converted ingress to gateway", "conversionErrors", conversionErrors)
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
