package targetcontroller

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-kube-storage-version-migrator-operator/pkg/operator/assets"
)

func (c *TargetController) syncKubeStorageVersionManager(spec *operatorv1.KubeStorageVersionMigratorSpec, originalOperatorStatus *operatorv1.KubeStorageVersionMigratorStatus, generation int64) (bool, error) {
	var errors []error
	operatorStatus := originalOperatorStatus.DeepCopy()

	directResourceResults := resourceapply.ApplyDirectly(c.kubeClient, c.eventRecorder, assets.Asset,
		"kube-storage-version-migrator/namespace.yaml",
		"kube-storage-version-migrator/serviceaccount.yaml",
		"kube-storage-version-migrator/roles.yaml",
	)

	for _, result := range directResourceResults {
		if result.Error != nil {
			errors = append(errors, fmt.Errorf("%q (%T): %v", result.File, result.Type, result.Error))
			continue
		}
	}

	deployment, _, err := c.manageKubeStorageVersionManagerDeployment(spec, operatorStatus)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "deployments", err))
	}

	manageOperatorStatusAvailable(deployment, operatorStatus)
	manageOperatorStatusProgressing(deployment, operatorStatus, generation)
	manageOperatorStatusDegraded(errors, operatorStatus)

	// TODO this is changing too early and it was before too.
	operatorStatus.ObservedGeneration = generation
	resourcemerge.SetDeploymentGeneration(&operatorStatus.Generations, deployment)

	// if we are available, we need to try to set our versions correctly.
	if v1helpers.IsOperatorConditionTrue(operatorStatus.Conditions, operatorv1.OperatorStatusTypeAvailable) {
		operandVersion := status.VersionForOperand(
			OperatorNamespace,
			deployment.Spec.Template.Spec.Containers[0].Image,
			c.kubeClient.CoreV1(),
			c.eventRecorder)
		c.versionRecorder.SetVersion("kube-storage-version-migrator", operandVersion)
	}

	_, _, err = v1helpers.UpdateStatus(c.genericOperatorConfigClient, func(oldStatus *operatorv1.OperatorStatus) error {
		for _, condition := range operatorStatus.Conditions {
			if err := v1helpers.UpdateConditionFn(condition)(oldStatus); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}

	if len(errors) > 0 {
		return true, nil
	}
	if !v1helpers.IsOperatorConditionFalse(operatorStatus.Conditions, operatorv1.OperatorStatusTypeDegraded) {
		return true, nil
	}
	if !v1helpers.IsOperatorConditionFalse(operatorStatus.Conditions, operatorv1.OperatorStatusTypeProgressing) {
		return true, nil
	}
	if !v1helpers.IsOperatorConditionTrue(operatorStatus.Conditions, operatorv1.OperatorStatusTypeAvailable) {
		return true, nil
	}

	return false, nil
}

func manageOperatorStatusAvailable(deployment *appsv1.Deployment, status *operatorv1.KubeStorageVersionMigratorStatus) {
	switch {
	case deployment == nil:
		v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeAvailable,
			Status:  operatorv1.ConditionFalse,
			Reason:  "NoDeployment",
			Message: "deployment/migrator.openshift-kube-storage-version-migrator: could not be retrieved",
		})
	case deployment.Status.AvailableReplicas == 0:
		v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeAvailable,
			Status:  operatorv1.ConditionFalse,
			Reason:  "NoMigratorPod",
			Message: "deployment/migrator.openshift-kube-storage-version-migrator: no replicas are available",
		})
	}
}

func manageOperatorStatusProgressing(deployment *appsv1.Deployment, status *operatorv1.KubeStorageVersionMigratorStatus, generation int64) {
	// If the deployment is up to date and the operatorConfig are up to date, then we are no longer progressing
	var progressingMessages []string
	if deployment != nil && deployment.ObjectMeta.Generation != deployment.Status.ObservedGeneration {
		progressingMessages = append(progressingMessages, fmt.Sprintf("deployment/migrator.openshift-kube-storage-version-migrator:: observed generation is %d, desired generation is %d.", deployment.Status.ObservedGeneration, deployment.ObjectMeta.Generation))
	}
	if generation != status.ObservedGeneration {
		progressingMessages = append(progressingMessages, fmt.Sprintf("kubestorageversionmigrators/cluster: observed generation is %d, desired generation is %d.", status.ObservedGeneration, generation))
	}

	if len(progressingMessages) == 0 {
		v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeProgressing,
			Status: operatorv1.ConditionFalse,
		})
	} else {
		v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeProgressing,
			Status:  operatorv1.ConditionTrue,
			Reason:  "DesiredStateNotYetAchieved",
			Message: strings.Join(progressingMessages, "\n"),
		})
	}
}

func manageOperatorStatusDegraded(errors []error, status *operatorv1.KubeStorageVersionMigratorStatus) {
	if len(errors) > 0 {
		message := ""
		for _, err := range errors {
			message = message + err.Error() + "\n"
		}
		v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
			Type:    "TargetDegraded",
			Status:  operatorv1.ConditionTrue,
			Message: message,
			Reason:  "SyncError",
		})
	} else {
		v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
			Type:   "TargetDegraded",
			Status: operatorv1.ConditionFalse,
		})
	}
}

func (c *TargetController) manageKubeStorageVersionManagerDeployment(spec *operatorv1.KubeStorageVersionMigratorSpec, status *operatorv1.KubeStorageVersionMigratorStatus) (*appsv1.Deployment, bool, error) {

	// load deployment
	deployment := resourceread.ReadDeploymentV1OrDie(assets.MustAsset("kube-storage-version-migrator/deployment.yaml"))

	// resolve image references
	var err error
	templateSpec := &deployment.Spec.Template.Spec
	if templateSpec.InitContainers, err = c.resolveImageReferences(templateSpec.InitContainers); err != nil {
		return nil, false, err
	}
	if templateSpec.Containers, err = c.resolveImageReferences(templateSpec.Containers); err != nil {
		return nil, false, err
	}

	// set annotations to force diff on pull spec changes
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	deployment.Annotations["kubestorageversionmigrators.operator.openshift.io/pull-spec"] = c.imagePullSpec
	deployment.Annotations["kubestorageversionmigrators.operator.openshift.io/operator-pull-spec"] = c.operatorImagePullSpec

	// set operand log level
	operandContainer := deployment.Spec.Template.Spec.Containers[0]
	operandContainer.Args = append(operandContainer.Args, fmt.Sprintf("--v=%d", klogLevels[spec.LogLevel]))

	return resourceapply.ApplyDeployment(c.kubeClient.AppsV1(), c.eventRecorder, deployment, resourcemerge.ExpectedDeploymentGeneration(deployment, status.Generations), false)
}

func (c *TargetController) resolveImageReferences(containers []corev1.Container) ([]corev1.Container, error) {
	var results []corev1.Container
	for _, container := range containers {
		switch container.Image {
		case "${IMAGE}":
			container.Image = c.imagePullSpec
		case "${OPERATOR_IMAGE}":
			container.Image = c.operatorImagePullSpec
		default:
			if strings.Contains(container.Image, "$") {
				return containers, fmt.Errorf("invalid image reference %q", container.Image)
			}
		}
		results = append(results, container)
	}
	return results, nil
}

var klogLevels = map[operatorv1.LogLevel]int{
	"":                  2,
	operatorv1.Normal:   2,
	operatorv1.Debug:    4,
	operatorv1.Trace:    6,
	operatorv1.TraceAll: 8,
}
