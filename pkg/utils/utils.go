package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	storagev1 "k8s.io/client-go/kubernetes/typed/storage/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/kubebuilder-declarative-pattern/pkg/patterns/declarative/pkg/manifest"
	kyaml "sigs.k8s.io/kustomize/kyaml/yaml"
	"sigs.k8s.io/yaml"
)

// NewClientConfig returns a client-go rest config
func NewClientConfig() (*rest.Config, error) {
	configFlags := &genericclioptions.ConfigFlags{}

	config, err := configFlags.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	return config, nil
}

// GetClientsetFromConfig returns a k8s clientset from a client-go rest config
func GetClientsetFromConfig(config *rest.Config) (*kubernetes.Clientset, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

// ExecToPod execs into a pod and executes command from inside that pod.
// containerName can be "" if the pod contains only a single container.
// Returned are strings represent STDOUT and STDERR respectively.
// Also returned is any error encountered.
func ExecToPod(config *rest.Config, command []string, containerName, podName, namespace string, stdin io.Reader) (string, string, error) {
	clientset, err := GetClientsetFromConfig(config)
	if err != nil {
		return "", "", err
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return "", "", fmt.Errorf("error adding to scheme: %v", err)
	}

	parameterCodec := runtime.NewParameterCodec(scheme)
	req.VersionedParams(&corev1.PodExecOptions{
		Command:   command,
		Container: containerName,
		Stdin:     stdin != nil,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, parameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("error while creating Executor: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		return "", "", fmt.Errorf("error in Stream: %v", err)
	}

	return stdout.String(), stderr.String(), nil
}

// GetDefaultStorageClassName returns the name of the default storage class in the cluster, if more
// than one storage class is set to default, the first one discovered is returned. An error is returned
// if no default storage class is found.
func GetDefaultStorageClassName() (string, error) {
	restConfig, err := NewClientConfig()
	if err != nil {
		return "", err
	}

	storageV1Client, err := storagev1.NewForConfig(restConfig)
	if err != nil {
		return "", err
	}
	storageClasses, err := storageV1Client.StorageClasses().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, storageClass := range storageClasses.Items {
		for k, v := range storageClass.GetObjectMeta().GetAnnotations() {
			if k == "storageclass.kubernetes.io/is-default-class" && v == "true" {
				return storageClass.GetObjectMeta().GetName(), nil
			}
		}
	}

	return "", fmt.Errorf("no default storage class discovered in cluster")
}

// GetManifestFromMultiDoc returns an individual object string from a multi-doc yaml file
// after searching by kind. Note: the first object in multiManifest matching kind is returned.
func GetManifestFromMultiDoc(multiManifest, kind string) (string, error) {
	objs, err := manifest.ParseObjects(context.TODO(), multiManifest)
	if err != nil {
		return "", err
	}
	for _, obj := range objs.Items {
		if obj.UnstructuredObject().GetKind() == kind {
			objYaml, err := yaml.Marshal(obj.UnstructuredObject())
			if err != nil {
				return "", err
			}
			return string(objYaml), nil
		}
	}
	return "", fmt.Errorf("no object of kind: %s found in multi doc manifest", kind)
}

// SetFieldInManifest sets valueName equal to value at path in manifest defined by fields.
// See TestSetFieldInManifest for examples.
func SetFieldInManifest(manifest, value, valueName string, fields ...string) (string, error) {
	obj, err := kyaml.Parse(manifest)
	if err != nil {
		return "", err
	}

	parsedVal, err := kyaml.Parse(value)
	if err != nil {
		return "", err
	}

	_, err = obj.Pipe(kyaml.LookupCreate(kyaml.MappingNode, fields...), kyaml.SetField(valueName, parsedVal))
	if err != nil {
		return "", err
	}
	return obj.MustString(), nil

}

// GetFieldInManifest returns the string value at path in manifest defined by fields.
// See TestGetFieldInManifest for examples.
func GetFieldInManifest(manifest string, fields ...string) (string, error) {
	obj, err := kyaml.Parse(manifest)
	if err != nil {
		return "", err
	}

	val, err := obj.Pipe(kyaml.Lookup(fields...))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(val.MustString()), nil
}

// KustomizePatch is useed to pass a new patch to a kustomization file, see AddPatchesToKustomize
type KustomizePatch struct {
	Op    string
	Path  string
	Value string
}

// AddPatchesToKustomize adds any number of patches (via []KustomizePatch{}) to kustomizationFile string,
// returning the updated kustomization file as a string.
// Example
//*******************************************************
// Input kustomization file:
//*******************************************************
// apiVersion: kustomize.config.k8s.io/v1beta1
// kind: Kustomization
//
// resources:
// - storageos-cluster.yaml
//******************************************************
// Other inputs:
// targetKind: "StorageOSCluster"
// targetName: "storageoscluster-sample"
// patches: []KustomizePatch{
//	{
//		Op: "replace",
//		Path: "/spec/kvBackend/address",
//		Value: 	"storageos.storageos-etcd:2379",
//	},
// }
//*******************************************************
// Results in the following output kustomization file:
//*******************************************************
// apiVersion: kustomize.config.k8s.io/v1beta1
// kind: Kustomization
//
// resources:
// - storageos-cluster.yaml
//
// patches:
// - target:
//     kind: StorageOSCluster
//     name: storageoscluster-sample
//   patch: |
//     - op: replace
//       path: /spec/kvBackend/address
//       value: storageos.storageos-etcd:2379
//*******************************************************
func AddPatchesToKustomize(kustomizationFile, targetKind, targetName string, patches []KustomizePatch) (string, error) {
	obj, err := kyaml.Parse(string(kustomizationFile))
	if err != nil {
		return "", err
	}

	patchStrings := make([]string, 0)
	for _, patch := range patches {
		patchString := fmt.Sprintf("%s%s%s%s%s%s", `
    - op: `, patch.Op, `
      path: `, patch.Path, `
      value: `, patch.Value)
		patchStrings = append(patchStrings, patchString)

	}

	allPatchesStr := strings.Join(patchStrings, "")

	targetString := fmt.Sprintf("%s%s%s%s%s", `
- target:
    kind: `, targetKind, `
    name: `, targetName, `
  patch: |`)

	patch, _ := kyaml.Parse(strings.Join([]string{targetString, allPatchesStr}, ""))

	_, err = obj.Pipe(
		kyaml.LookupCreate(kyaml.SequenceNode, "patches"),
		kyaml.Append(patch.YNode().Content...))
	if err != nil {
		return "", err
	}

	return obj.MustString(), nil
}

// GenericPatchesForSupportBundle creates and returns []KustomizePatch for a kustomiziation file to be applied to the
// SupportBundle.
//
// Inputs:
// * spec: string of the SupportBundle manifest
// * instruction: "collectors" or "analyzers"
// * value: string of Value for patch
// * fields: path of fields (after instruction) to value to be changed in SupportBundle eg {"namespace"}
// * lookUpValue: value to compare at path skipByFields eg "storageos-operator-logs". If lookup value is left empty,
// any instruction with skipByFields path is skipped. This value is only to specify a single instruction for ignoring.
// * pathsToSkip: (optional) include paths of fields for an instructions to be ignored (ie no patch applied even if it
// matches 'fields' path above. Eg {{"logs"},{"run"}}
//
// This function is useful in cases where it is desired to set a field such as namespace in a SupportBundle for most
// (but not all instructions). The appropriate patches are created and can then be added to the applicable kustomization.
func GenericPatchesForSupportBundle(spec, instruction, value string, fields []string, skipLookUpValue string, pathsToSkip [][]string) ([]KustomizePatch, error) {
	instructionTypes, err := getSupportBundleInstructionTypes(instruction)
	if err != nil {
		return nil, err
	}

	obj, err := kyaml.Parse(spec)
	if err != nil {
		return nil, err
	}
	instructionObj, err := obj.Pipe(kyaml.Lookup(
		"spec",
		instruction,
	))
	if err != nil {
		return nil, err
	}
	instructionPatches := make([]KustomizePatch, 0)
	elements, _ := instructionObj.Elements()
	for count, element := range elements {
		skipElement, err := skipElement(element, pathsToSkip, skipLookUpValue)
		if err != nil {
			return nil, err
		}
		if skipElement {
			continue
		}
		for _, instructionType := range instructionTypes {

			instructionNode, err := element.Pipe(kyaml.Lookup(instructionType))
			if err != nil {
				return nil, err
			}
			if instructionNode == nil {
				continue
			}

			fieldNode, err := instructionNode.Pipe(kyaml.Lookup(fields...))
			if err != nil {

				return nil, err
			}
			if fieldNode == nil {
				break
			}
			path := filepath.Join("/spec", instruction, strconv.Itoa(count), instructionType, filepath.Join(fields...))
			instructionPatches = append(instructionPatches, KustomizePatch{Op: "replace", Path: path, Value: value})
		}
	}
	return instructionPatches, nil
}

// skipElemnt is a helper function for GenericPatchesForSupportBundle - it decides whether or not and
// instruction should be skipped based on whether pathsToSkip and/or lookUpValue exists within the instruction.
func skipElement(element *kyaml.RNode, pathsToSkip [][]string, lookUpValue string) (bool, error) {
	for _, pathToSkip := range pathsToSkip {
		if len(pathToSkip) == 0 {
			continue
		}
		elementNodeToSkip, err := element.Pipe(kyaml.Lookup(pathToSkip...))
		if err != nil {
			return false, err
		}
		if lookUpValue == "" {
			if elementNodeToSkip != nil {
				return true, nil
			}
		} else {
			if strings.TrimSpace(elementNodeToSkip.MustString()) == strings.TrimSpace(lookUpValue) {
				return true, nil
			}
		}
	}
	return false, nil
}

// SpecificPatchForSupportBundle creates and returns KustomizePatch for a kustomiziation file to be applied to the
// SupportBundle.
//
// Inputs:
// * spec: string of the SupportBundle manifest
// * instruction: "collectors" or "analyzers"
// * value: string of Value for patch
// * fields: path of fields (after instruction) to value to be changed in SupportBundle eg {"run","namespace"}
// * lookUpValue: value to compare at path findByFields eg "storageos-operator-logs"
// * findByFields: path of fields to locate the specific instruction
// eg {"logs","name"}
//
// This function is useful in cases where it is desired to set a field such as namespace in a SupportBundle for a
// specific collector or analyzer
func SpecificPatchForSupportBundle(spec, instruction, value string, fields []string, lookUpValue string, findByFields []string) (KustomizePatch, error) {
	kPatch := KustomizePatch{}
	obj, err := kyaml.Parse(spec)
	if err != nil {
		return kPatch, err
	}
	instructionObj, err := obj.Pipe(kyaml.Lookup(
		"spec",
		instruction,
	))
	if err != nil {
		return kPatch, err
	}

	elements, _ := instructionObj.Elements()
	for count, element := range elements {
		if len(findByFields) != 0 {
			elementNodeToPatch, err := element.Pipe(kyaml.Lookup(findByFields...))
			if err != nil {
				return kPatch, err
			}
			if strings.TrimSpace(elementNodeToPatch.MustString()) != strings.TrimSpace(lookUpValue) {
				continue
			}
		}
		path := filepath.Join("/spec", instruction, strconv.Itoa(count), filepath.Join(fields...))
		return KustomizePatch{Op: "replace", Value: value, Path: path}, nil
	}
	return kPatch, fmt.Errorf("path not found in support bundle")
}

// AllInstructionTypesExcept returns [][]string of all instructino types for instruction, except for those provided
func AllInstructionTypesExcept(instruction string, exceptions ...string) ([][]string, error) {
	allTypes, err := getSupportBundleInstructionTypes(instruction)
	if err != nil {
		return nil, err
	}
	finalInstructionTypes := make([][]string, 0)
	for _, instructionType := range allTypes {
		exists := false
		for _, exception := range exceptions {
			if instructionType == exception {
				exists = true
			}
		}
		if exists {
			continue
		}
		single := []string{instructionType}
		finalInstructionTypes = append(finalInstructionTypes, single)
	}

	return finalInstructionTypes, nil
}

// getSupportBundleInstructinoTypes returns the list of types for analyzer or collector instructions
func getSupportBundleInstructionTypes(instruction string) ([]string, error) {
	collectorTypes := []string{
		"clusterInfo",
		"clusterResources",
		"logs",
		"copy",
		"data",
		"secret",
		"run",
		"http",
		"exec",
		"postgresql",
		"mysql",
		"redis",
		"ceph",
		"longhorn",
		"registryImages",
	}
	analyzerTypes := []string{
		"clusterVersion",
		"distribution",
		"containerRuntime",
		"nodeResources",
		"deploymentStatus",
		"statefulsetStatus",
		"imagePullSecret",
		"ingress",
		"storageClass",
		"secret",
		"customResourceDefinition",
		"textAnalyze",
		"postgres",
		"mysql",
		"cephStatus",
		"longhorn",
		"registryImages",
	}

	switch instruction {
	case "collectors":
		return collectorTypes, nil
	case "analyzers":
		return analyzerTypes, nil
	default:
		return nil, fmt.Errorf("unsupported instruction %v, must be \"collectors\" or \"analyzers\"", instruction)
	}

}

// NamespaceYaml returns a yaml string for a namespace object based on the namespace name
func NamespaceYaml(namespace string) string {
	return fmt.Sprintf("%v%v", `apiVersion: v1
kind: Namespace
metadata:
  name: `, namespace)

}

// PodIsRunning attempts to `get` a pod by name and namespace, the function returns no error
// if the pod is in running phase. If an error occurs during `get`, the error is returned.
// If the pod is a phase other than running, `get` is executed again after `interval` seconds.
// After `limit` seconds, the function times out and returns timeout error.
func PodIsRunning(config *rest.Config, name, namespace string, limit, interval time.Duration) error {
	clientset, err := GetClientsetFromConfig(config)
	if err != nil {
		return err
	}
	podClient := clientset.CoreV1().Pods(namespace)
	timeout := time.After(time.Second * limit)
	errs, ctx := errgroup.WithContext(context.TODO())
	errs.Go(func() error {
		for {
			select {
			case <-timeout:
				return fmt.Errorf("timeout attempting to reach pod %s;%s", name, namespace)
			default:
				pod, err := podClient.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if pod.Status.Phase == corev1.PodRunning {
					return nil
				}
				time.Sleep(interval * time.Second)
			}
		}
	})
	return errs.Wait()
}

// DeploymentIsReady attempts to `get` a deployment by name and namespace, the function returns no error
// if the deployment replicas are all ready. If an error occurs during `get`, the error is returned.
// If the deployment replicas are not all ready, `get` is executed again after `interval` seconds.
// After `limit` seconds, the function times out and returns timeout error.
func DeploymentIsReady(config *rest.Config, name, namespace string, limit, interval time.Duration) error {
	clientset, err := GetClientsetFromConfig(config)
	if err != nil {
		return err
	}
	depClient := clientset.AppsV1().Deployments(namespace)
	timeout := time.After(time.Second * limit)
	errs, ctx := errgroup.WithContext(context.TODO())
	errs.Go(func() error {
		for {
			select {
			case <-timeout:
				return fmt.Errorf("timeout attempting to reach deployment %s;%s", name, namespace)
			default:
				dep, err := depClient.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if *dep.Spec.Replicas == dep.Status.ReadyReplicas {
					return nil
				}
				time.Sleep(interval * time.Second)
			}
		}
	})
	return errs.Wait()
}
