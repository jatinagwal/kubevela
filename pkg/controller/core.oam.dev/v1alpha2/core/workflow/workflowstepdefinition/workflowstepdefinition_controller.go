/*

 Copyright 2021 The KubeVela Authors.

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

package workflowstepdefinition

import (
	"context"
	"fmt"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/condition"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	common2 "github.com/oam-dev/kubevela/pkg/controller/common"
	oamctrl "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	coredef "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/version"
)

// Reconciler reconciles a WorkflowStepDefinition object
type Reconciler struct {
	client.Client
	dm     discoverymapper.DiscoveryMapper
	Scheme *runtime.Scheme
	record event.Recorder
	options
}

type options struct {
	defRevLimit          int
	concurrentReconciles int
	ignoreDefNoCtrlReq   bool
	controllerVersion    string
}

// Reconcile is the main logic for WorkflowStepDefinition controller
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := common2.NewReconcileContext(ctx)
	defer cancel()

	definitionName := req.NamespacedName.Name
	klog.InfoS("Reconciling WorkflowStepDefinition...", "Name", definitionName, "Namespace", req.Namespace)

	var wfStepDefinition v1beta1.WorkflowStepDefinition
	if err := r.Get(ctx, req.NamespacedName, &wfStepDefinition); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// this is a placeholder for finalizer here in the future
	if wfStepDefinition.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	if !coredef.MatchControllerRequirement(&wfStepDefinition, r.controllerVersion, r.ignoreDefNoCtrlReq) {
		klog.InfoS("skip definition: not match the controller requirement of definition", "workflowStepDefinition", klog.KObj(&wfStepDefinition))
		return ctrl.Result{}, nil
	}

	defRev, result, err := coredef.ReconcileDefinitionRevision(ctx, r.Client, r.record, &wfStepDefinition, r.defRevLimit, func(revision *common.Revision) error {
		wfStepDefinition.Status.LatestRevision = revision
		if err := r.UpdateStatus(ctx, &wfStepDefinition); err != nil {
			return err
		}
		return nil
	})
	if result != nil {
		return *result, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	def := utils.NewCapabilityStepDef(&wfStepDefinition)
	def.Name = req.NamespacedName.Name
	// Store the parameter of stepDefinition to configMap
	cmName, err := def.StoreOpenAPISchema(ctx, r.Client, req.Namespace, req.Name, defRev.Name)
	if err != nil {
		klog.InfoS("Could not store capability in ConfigMap", "err", err)
		r.record.Event(&(wfStepDefinition), event.Warning("Could not store capability in ConfigMap", err))
		return ctrl.Result{}, util.PatchCondition(ctx, r, &wfStepDefinition,
			condition.ReconcileError(fmt.Errorf(util.ErrStoreCapabilityInConfigMap, wfStepDefinition.Name, err)))
	}

	if wfStepDefinition.Status.ConfigMapRef != cmName {
		wfStepDefinition.Status.ConfigMapRef = cmName
		if err := r.UpdateStatus(ctx, &wfStepDefinition); err != nil {
			klog.ErrorS(err, "Could not update WorkflowStepDefinition Status", "workflowStepDefinition", klog.KRef(req.Namespace, req.Name))
			r.record.Event(&wfStepDefinition, event.Warning("Could not update WorkflowStepDefinition Status", err))
			return ctrl.Result{}, util.PatchCondition(ctx, r, &wfStepDefinition,
				condition.ReconcileError(fmt.Errorf(util.ErrUpdateWorkflowStepDefinition, wfStepDefinition.Name, err)))
		}
		klog.InfoS("Successfully updated the status.configMapRef of the WorkflowStepDefinition", "workflowStepDefinition",
			klog.KRef(req.Namespace, req.Name), "status.configMapRef", cmName)
	}
	return ctrl.Result{}, nil
}

// UpdateStatus updates v1beta1.WorkflowStepDefinition's Status with retry.RetryOnConflict
func (r *Reconciler) UpdateStatus(ctx context.Context, def *v1beta1.WorkflowStepDefinition, opts ...client.UpdateOption) error {
	status := def.DeepCopy().Status
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		if err = r.Get(ctx, client.ObjectKey{Namespace: def.Namespace, Name: def.Name}, def); err != nil {
			return
		}
		def.Status = status
		return r.Status().Update(ctx, def, opts...)
	})
}

// SetupWithManager will setup with event recorder
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.record = event.NewAPIRecorder(mgr.GetEventRecorderFor("WorkflowStepDefinition")).
		WithAnnotations("controller", "WorkflowStepDefinition")
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.concurrentReconciles,
		}).
		For(&v1beta1.WorkflowStepDefinition{}).
		Complete(r)
}

// Setup adds a controller that reconciles WorkflowStepDefinition.
func Setup(mgr ctrl.Manager, args oamctrl.Args) error {
	r := Reconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		dm:      args.DiscoveryMapper,
		options: parseOptions(args),
	}
	return r.SetupWithManager(mgr)
}

func parseOptions(args oamctrl.Args) options {
	return options{
		defRevLimit:          args.DefRevisionLimit,
		concurrentReconciles: args.ConcurrentReconciles,
		ignoreDefNoCtrlReq:   args.IgnoreDefinitionWithoutControllerRequirement,
		controllerVersion:    version.VelaVersion,
	}
}
