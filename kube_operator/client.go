package kube_operator

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/cutil"
	olmv1scheme "github.com/operator-framework/api/pkg/operators/v1"
	olmv1alpha1scheme "github.com/operator-framework/api/pkg/operators/v1alpha1"
	olmv1client "github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned/typed/operators/v1"
	olmv1alpha1client "github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned/typed/operators/v1alpha1"
	yaml "gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	v1scheme "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1beta1scheme "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	dynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"reflect"
	"strings"
)

const (
	DEFAULT_ANAX_NAMESPACE = "openhorizon-agent"
	// Name for the env var config map. Only characters allowed: [a-z] "." and "-"
	HZN_ENV_VARS = "hzn-env-vars"
	// Variable that contains the name of the config map
	HZN_ENV_KEY = "HZN_ENV_VARS"

	K8S_ROLE_TYPE               = "Role"
	K8S_ROLEBINDING_TYPE        = "RoleBinding"
	K8S_DEPLOYMENT_TYPE         = "Deployment"
	K8S_SERVICEACCOUNT_TYPE     = "ServiceAccount"
	K8S_CRD_TYPE                = "CustomResourceDefinition"
	K8S_NAMESPACE_TYPE          = "Namespace"
	K8S_UNSTRUCTURED_TYPE       = "Unstructured"
	K8S_OLM_OPERATOR_GROUP_TYPE = "OperatorGroup"
)

func getBaseK8sKinds() []string {
	return []string{K8S_NAMESPACE_TYPE, K8S_ROLE_TYPE, K8S_ROLEBINDING_TYPE, K8S_DEPLOYMENT_TYPE, K8S_SERVICEACCOUNT_TYPE, K8S_CRD_TYPE}
}

func getDangerKinds() []string {
	return []string{K8S_OLM_OPERATOR_GROUP_TYPE}
}

func IsBaseK8sType(kind string) bool {
	return cutil.SliceContains(getBaseK8sKinds(), kind)
}

func IsDangerType(kind string) bool {
	return cutil.SliceContains(getDangerKinds(), kind)
}

// Intermediate state for the objects used for k8s api objects that haven't had their exact type asserted yet
type APIObjects struct {
	Type   *schema.GroupVersionKind
	Object interface{}
}

// Intermediate state used for after the objects have been read from the deployment but not converted to k8s objects yet
type YamlFile struct {
	Header tar.Header
	Body   string
}

// Client to interact with all standard k8s objects
type KubeClient struct {
	Client            *kubernetes.Clientset
	DynClient         dynamic.Interface
	OLMV1Alpha1Client olmv1alpha1client.OperatorsV1alpha1Client
	OLMV1Client       olmv1client.OperatorsV1Client
}

// KubeStatus contains the status of operator pods and a user-defined status object
type KubeStatus struct {
	ContainerStatuses []ContainerStatus
	OperatorStatus    interface{}
}

type ContainerStatus struct {
	Name        string
	Image       string
	CreatedTime int64
	State       string
}

func NewKubeClient() (*KubeClient, error) {
	clientset, err := cutil.NewKubeClient()
	if err != nil {
		return nil, err
	}
	dynClient, err := NewDynamicKubeClient()
	if err != nil {
		return nil, err
	}
	return &KubeClient{Client: clientset, DynClient: dynClient}, nil
}

// NewDynamicKubeClient returns a kube client that interacts with unstructured.Unstructured type objects
func NewDynamicKubeClient() (dynamic.Interface, error) {
	config, err := cutil.NewKubeConfig()
	if err != nil {
		return nil, err
	}
	clientset, _ := dynamic.NewForConfig(config)
	return clientset, nil
}

// Install creates the objects specified in the operator deployment in the cluster and creates the custom resource to start the operator
func (c KubeClient) Install(tar string, metadata map[string]interface{}, envVars map[string]string, agId string, reqNamespace string, crInstallTimeout int64) error {

	apiObjMap, opNamespace, err := ProcessDeployment(tar, metadata, envVars, agId, crInstallTimeout)
	if err != nil {
		return err
	}

	// get and check namespace
	namespace := getFinalNamespace(reqNamespace, opNamespace)
	nodeNamespace := cutil.GetClusterNamespace()
	if namespace != nodeNamespace && nodeNamespace != DEFAULT_ANAX_NAMESPACE {
		return fmt.Errorf("Service failed to start for agreement %v. Could not deploy service into namespace %v because the agent's namespace is %v and it restricts all services to have the same namespace.", agId, namespace, nodeNamespace)
	}

	// If the namespace was specified in the deployment then create the namespace object so it can be created
	if _, ok := apiObjMap[K8S_NAMESPACE_TYPE]; !ok && namespace != nodeNamespace {
		nsObj := corev1.Namespace{TypeMeta: metav1.TypeMeta{Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		apiObjMap[K8S_NAMESPACE_TYPE] = []APIObjectInterface{NamespaceCoreV1{NamespaceObject: &nsObj}}
	}

	baseK8sComponents := getBaseK8sKinds()

	// install all the objects of built-in k8s types
	for _, componentType := range baseK8sComponents {
		for _, componentObj := range apiObjMap[componentType] {
			if err = componentObj.Install(c, namespace); err != nil {
				return err
			}
			glog.Infof(kwlog(fmt.Sprintf("successfully installed %v %v", componentType, componentObj.Name())))
		}
	}

	// install any remaining components of unknown type
	for _, unknownObj := range apiObjMap[K8S_UNSTRUCTURED_TYPE] {
		if err = unknownObj.Install(c, namespace); err != nil {
			return err
		}
		glog.Infof(kwlog(fmt.Sprintf("successfully installed %v", unknownObj.Name())))
	}

	glog.V(3).Infof(kwlog(fmt.Sprintf("all operator objects installed")))

	return nil
}

// Install creates the objects specified in the operator deployment in the cluster and creates the custom resource to start the operator
func (c KubeClient) Uninstall(tar string, metadata map[string]interface{}, agId string, reqNamespace string) error {

	apiObjMap, opNamespace, err := ProcessDeployment(tar, metadata, map[string]string{}, agId, 0)
	if err != nil {
		return err
	}
	namespace := getFinalNamespace(reqNamespace, opNamespace)

	for _, crd := range apiObjMap[K8S_CRD_TYPE] {
		crd.Uninstall(c, namespace)
	}

	baseK8sComponents := getBaseK8sKinds()

	// uninstall all the objects of built-in k8s types
	for i := len(baseK8sComponents) - 1; i >= 0; i-- {
		componentType := baseK8sComponents[i]
		for _, componentObj := range apiObjMap[componentType] {
			glog.Infof(kwlog(fmt.Sprintf("attempting to uninstall %v %v", componentType, componentObj.Name())))
			componentObj.Uninstall(c, namespace)
		}
	}

	// uninstall any remaining components of unknown type
	for _, unknownObj := range apiObjMap[K8S_UNSTRUCTURED_TYPE] {
		glog.Infof(kwlog(fmt.Sprintf("attempting to uninstall %v", unknownObj.Name())))
		unknownObj.Uninstall(c, namespace)
	}

	glog.V(3).Infof(kwlog(fmt.Sprintf("Completed removal of all operator objects from the cluster.")))
	return nil
}
func (c KubeClient) OperatorStatus(tar string, metadata map[string]interface{}, agId string, reqNamespace string) (interface{}, error) {
	apiObjMap, opNamespace, err := ProcessDeployment(tar, metadata, map[string]string{}, agId, 0)
	if err != nil {
		return nil, err
	}
	namespace := getFinalNamespace(reqNamespace, opNamespace)

	if len(apiObjMap[K8S_DEPLOYMENT_TYPE]) < 1 {
		return nil, fmt.Errorf(kwlog(fmt.Sprintf("Error: failed to find operator deployment object.")))
	}

	status, err := apiObjMap[K8S_DEPLOYMENT_TYPE][0].Status(c, namespace)
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (c KubeClient) Status(tar string, metadata map[string]interface{}, agId string, reqNamespace string) ([]ContainerStatus, error) {
	apiObjMap, opNamespace, err := ProcessDeployment(tar, metadata, map[string]string{}, agId, 0)
	if err != nil {
		return nil, err
	}
	namespace := getFinalNamespace(reqNamespace, opNamespace)

	if len(apiObjMap[K8S_DEPLOYMENT_TYPE]) < 1 {
		return nil, fmt.Errorf(kwlog(fmt.Sprintf("Error: failed to find operator deployment object.")))
	}

	deployment := apiObjMap[K8S_DEPLOYMENT_TYPE][0]

	podList, err := deployment.Status(c, namespace)
	if err != nil {
		return nil, err
	}

	if podListTyped, ok := podList.(*corev1.PodList); ok {
		if len(podListTyped.Items) < 1 {
			return nil, nil
		}
		pod := podListTyped.Items[0]
		containerStatuses := []ContainerStatus{}

		for _, status := range pod.Status.ContainerStatuses {
			newStatus := ContainerStatus{Name: pod.ObjectMeta.Name}
			newStatus.Image = status.Image
			newStatus.Name = status.Name
			if status.State.Running != nil {
				newStatus.State = "Running"
				newStatus.CreatedTime = status.State.Running.StartedAt.Time.Unix()
			} else if status.State.Terminated != nil {
				newStatus.State = "Terminated"
				newStatus.CreatedTime = status.State.Terminated.StartedAt.Time.Unix()
			} else {
				newStatus.State = "Waiting"
			}
			containerStatuses = append(containerStatuses, newStatus)
		}
		return containerStatuses, nil
	} else {
		return nil, fmt.Errorf(kwlog(fmt.Sprintf("Error: deployment status returned unexpected type.")))
	}
}

// processDeployment takes the deployment string and converts it to a map with the k8s objects, the namespace to be used, and an error if one occurs
func ProcessDeployment(tar string, metadata map[string]interface{}, envVars map[string]string, agId string, crInstallTimeout int64) (map[string][]APIObjectInterface, string, error) {
	// Read the yaml files from the commpressed tar files
	yamls, err := getYamlFromTarGz(tar)
	if err != nil {
		return nil, "", err
	}

	// Convert the yaml files to kubernetes objects
	k8sObjs, customResources, err := getK8sObjectFromYaml(yamls, nil)
	if err != nil {
		return nil, "", err
	}

	customResourceKindMap := map[string][]*unstructured.Unstructured{}
	for _, customResource := range customResources {
		unstructCr, err := unstructuredObjectFromYaml(customResource)
		if err != nil {
			return nil, "", err
		}
		customResourceKindMap[unstructCr.GetKind()] = append(customResourceKindMap[unstructCr.GetKind()], unstructCr)
	}

	// Sort the k8s api objects by kind
	return sortAPIObjects(k8sObjs, customResourceKindMap, metadata, envVars, agId, crInstallTimeout)
}

// CreateConfigMap will create a config map with the provided environment variable map
func (c KubeClient) CreateConfigMap(envVars map[string]string, agId string, namespace string) (string, error) {
	// a userinput with an empty string for the name will cause an error. need to remove before creating the configmap
	for varName, varVal := range envVars {
		if varName == "" {
			glog.Errorf("Omitting userinput with empty name and value: %v", varVal)
		}
		delete(envVars, "")
	}
	hznEnvConfigMap := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s", HZN_ENV_VARS, agId)}, Data: envVars}
	res, err := c.Client.CoreV1().ConfigMaps(namespace).Create(context.Background(), &hznEnvConfigMap, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("Error: failed to create config map for %s: %v", agId, err)
	}
	return res.ObjectMeta.Name, nil
}

func unstructuredObjectFromYaml(crStr YamlFile) (*unstructured.Unstructured, error) {
	cr := make(map[string]interface{})
	err := yaml.UnmarshalStrict([]byte(crStr.Body), &cr)
	if err != nil {
		return nil, fmt.Errorf(kwlog(fmt.Sprintf("Error unmarshaling custom resource in deployment. %v", err)))
	}

	newCr := makeAllKeysStrings(cr).(map[string]interface{})
	unstructCr := unstructured.Unstructured{Object: newCr}
	return &unstructCr, nil
}

// add a reference to the envvar config map to the deployment
func addConfigMapVarToDeploymentObject(deployment appsv1.Deployment, configMapName string) appsv1.Deployment {
	hznEnvVar := corev1.EnvVar{Name: HZN_ENV_KEY, Value: configMapName}
	i := len(deployment.Spec.Template.Spec.Containers) - 1
	for i >= 0 {
		newEnv := append(deployment.Spec.Template.Spec.Containers[i].Env, hznEnvVar)
		deployment.Spec.Template.Spec.Containers[i].Env = newEnv
		i--
	}
	return deployment
}

// recursively go over the given interface to ensure any map keys are strings
func makeAllKeysStrings(unmarshYaml interface{}) interface{} {
	if reflect.ValueOf(unmarshYaml).Kind() == reflect.Map {
		retMap := make(map[string]interface{})
		if interfKeyMap, ok := unmarshYaml.(map[interface{}]interface{}); ok {
			for key, value := range interfKeyMap {
				retMap[fmt.Sprintf("%v", key)] = makeAllKeysStrings(value)
			}
		} else {
			for key, value := range unmarshYaml.(map[string]interface{}) {
				retMap[key] = makeAllKeysStrings(value)
			}
		}
		return retMap
	} else if reflect.ValueOf(unmarshYaml).Kind() == reflect.Slice {
		correctedSlice := make([]interface{}, len(unmarshYaml.([]interface{})))
		for _, elem := range unmarshYaml.([]interface{}) {
			correctedSlice = append(correctedSlice, makeAllKeysStrings(elem))
		}
		return correctedSlice
	}
	return unmarshYaml
}

// Convert the given yaml files into k8s api objects
func getK8sObjectFromYaml(yamlFiles []YamlFile, sch *runtime.Scheme) ([]APIObjects, []YamlFile, error) {
	retObjects := []APIObjects{}
	customResources := []YamlFile{}

	if sch == nil {
		sch = runtime.NewScheme()
	}

	// This is required to allow the schema to recognize custom resource definition types
	_ = v1beta1scheme.AddToScheme(sch)
	_ = v1scheme.AddToScheme(sch)
	_ = scheme.AddToScheme(sch)
	_ = olmv1alpha1scheme.AddToScheme(sch)
	_ = olmv1scheme.AddToScheme(sch)

	// multiple yaml files can be in one file separated by '---'
	// these are split here and rejoined with the single files
	indivYamls := []YamlFile{}
	for _, file := range yamlFiles {
		if multFiles := strings.Split(file.Body, "---"); len(multFiles) > 1 {
			for _, indivYaml := range multFiles {
				if strings.TrimSpace(indivYaml) != "" {
					indivYamls = append(indivYamls, YamlFile{Body: indivYaml})
				}
			}
		} else {
			indivYamls = append(indivYamls, file)
		}
	}

	for _, fileStr := range indivYamls {
		decode := serializer.NewCodecFactory(sch).UniversalDecoder(v1beta1scheme.SchemeGroupVersion, v1scheme.SchemeGroupVersion, rbacv1.SchemeGroupVersion, appsv1.SchemeGroupVersion, corev1.SchemeGroupVersion, olmv1alpha1scheme.SchemeGroupVersion, olmv1scheme.SchemeGroupVersion).Decode
		obj, gvk, err := decode([]byte(fileStr.Body), nil, nil)

		if err != nil {
			customResources = append(customResources, fileStr)
		} else if IsBaseK8sType(gvk.Kind) {
			newObj := APIObjects{Type: gvk, Object: obj}
			retObjects = append(retObjects, newObj)
		} else if IsDangerType(gvk.Kind) {
			// the scheme has recognized this type but does not provide the function for converting it to an unstructured object. skip this one to avoid a panic.
			glog.Errorf(kwlog(fmt.Sprintf("Skipping unsupported kind %v", gvk.Kind)))
		} else {
			newUnstructObj := unstructured.Unstructured{}
			err = sch.Convert(obj, &newUnstructObj, conversion.Meta{})
			if err != nil {
				glog.Errorf("Err converting object to unstructured: %v", err)
			}
			newObj := APIObjects{Type: gvk, Object: &newUnstructObj}
			retObjects = append(retObjects, newObj)
		}
	}

	return retObjects, customResources, nil
}

// Read the compressed tar file from the operator deployments section
func getYamlFromTarGz(deploymentString string) ([]YamlFile, error) {
	files := []YamlFile{}

	archiveData, err := base64.StdEncoding.DecodeString(deploymentString)
	if err != nil {
		return files, err
	}
	r := strings.NewReader(string(archiveData))

	zipReader, err := gzip.NewReader(r)
	if err != nil {
		return files, err
	}
	tarReader := tar.NewReader(zipReader)

	for {
		header, err := tarReader.Next()

		if err == io.EOF || header == nil {
			break
		} else if header.Typeflag == tar.TypeDir {
			continue
		} else if err == nil {
			tar, err := ioutil.ReadAll(tarReader)
			if err != nil {
				return files, fmt.Errorf("Error reading tar file: %v", err)
			}
			newFile := YamlFile{Header: *header, Body: string(tar)}
			files = append(files, newFile)
		} else {
			return files, err
		}
	}
	return files, nil
}

// get the namespace that the service will eventually be deployed to.
// reqNamespace: the requested namespace fromt agbot. It the namespace specified
// in the pattern or policy. If it is empty, agbot assign it to the namespace embedded
// in the metadata attribute of the clusterDeployment string for a service.
// opNamespace: the namespace embedded in the service operator.
// The result namespace is:
//  1. reqNamespace if not empty,
//  2. opNamespace if not empty,
//  3. nodeNamespace.
func getFinalNamespace(reqNamespace string, opNamespace string) string {
	nodeNamespace := cutil.GetClusterNamespace()

	namespace := reqNamespace
	if namespace == "" {
		namespace = opNamespace
	}
	if namespace == "" {
		namespace = nodeNamespace
	}

	return namespace
}
