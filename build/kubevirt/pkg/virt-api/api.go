/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package virt_api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"

	restful "github.com/emicklei/go-restful"
	restfulspec "github.com/emicklei/go-restful-openapi"
	"github.com/go-openapi/spec"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	apiregistrationv1beta1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1beta1"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"

	webhooksutils "kubevirt.io/kubevirt/pkg/util/webhooks"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	clientutil "kubevirt.io/client-go/util"
	virtversion "kubevirt.io/client-go/version"
	"kubevirt.io/kubevirt/pkg/certificates/triple"
	"kubevirt.io/kubevirt/pkg/certificates/triple/cert"
	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/healthz"
	"kubevirt.io/kubevirt/pkg/rest/filter"
	"kubevirt.io/kubevirt/pkg/service"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/util/openapi"
	"kubevirt.io/kubevirt/pkg/virt-api/rest"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
	mutating_webhook "kubevirt.io/kubevirt/pkg/virt-api/webhooks/mutating-webhook"
	validating_webhook "kubevirt.io/kubevirt/pkg/virt-api/webhooks/validating-webhook"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
)

const (
	// Default port that virt-api listens on.
	defaultPort = 443

	// Default address that virt-api listens on.
	defaultHost = "0.0.0.0"

	// selfsigned cert secret name
	virtApiCertSecretName = "kubevirt-virt-api-certs"

	virtWebhookValidator = "virt-api-validator"
	virtWebhookMutator   = "virt-api-mutator"

	virtApiServiceName = "virt-api"

	vmiCreateValidatePath       = "/virtualmachineinstances-validate-create"
	vmiUpdateValidatePath       = "/virtualmachineinstances-validate-update"
	vmValidatePath              = "/virtualmachines-validate"
	vmirsValidatePath           = "/virtualmachinereplicaset-validate"
	vmipresetValidatePath       = "/vmipreset-validate"
	migrationCreateValidatePath = "/migration-validate-create"
	migrationUpdateValidatePath = "/migration-validate-update"

	vmMutatePath        = "/virtualmachines-mutate"
	vmiMutatePath       = "/virtualmachineinstances-mutate"
	migrationMutatePath = "/migration-mutate-create"

	certBytesValue        = "cert-bytes"
	keyBytesValue         = "key-bytes"
	signingCertBytesValue = "signing-cert-bytes"

	defaultConsoleServerPort = 8186
)

type VirtApi interface {
	Compose()
	Run()
	AddFlags()
	ConfigureOpenAPIService()
	Execute()
}

type virtAPIApp struct {
	service.ServiceListen
	SwaggerUI        string
	SubresourcesOnly bool
	virtCli          kubecli.KubevirtClient
	aggregatorClient *aggregatorclient.Clientset
	authorizor       rest.VirtApiAuthorizor
	certsDirectory   string
	clusterConfig    *virtconfig.ClusterConfig

	signingCertBytes  []byte
	certBytes         []byte
	keyBytes          []byte
	namespace         string
	tlsConfig         *tls.Config
	consoleServerPort int
}

var _ service.Service = &virtAPIApp{}

func NewVirtApi() VirtApi {

	app := &virtAPIApp{}
	app.BindAddress = defaultHost
	app.Port = defaultPort

	return app
}

func (app *virtAPIApp) Execute() {
	virtCli, err := kubecli.GetKubevirtClient()
	if err != nil {
		panic(err)
	}

	authorizor, err := rest.NewAuthorizor()
	if err != nil {
		panic(err)
	}

	config, err := kubecli.GetConfig()
	if err != nil {
		panic(err)
	}

	app.aggregatorClient = aggregatorclient.NewForConfigOrDie(config)

	app.authorizor = authorizor

	app.virtCli = virtCli

	app.certsDirectory, err = ioutil.TempDir("", "certsdir")
	if err != nil {
		panic(err)
	}
	app.namespace, err = clientutil.GetNamespace()
	if err != nil {
		panic(err)
	}

	app.Compose()
	app.ConfigureOpenAPIService()
	app.Run()
}

func subresourceAPIGroup() metav1.APIGroup {
	apiGroup := metav1.APIGroup{
		Name: "subresource.kubevirt.io",
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: v1.SubresourceGroupVersions[0].Group + "/" + v1.SubresourceGroupVersions[0].Version,
			Version:      v1.SubresourceGroupVersions[0].Version,
		},
	}

	for _, version := range v1.SubresourceGroupVersions {
		apiGroup.Versions = append(apiGroup.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: version.Group + "/" + version.Version,
			Version:      version.Version,
		})
	}
	apiGroup.ServerAddressByClientCIDRs = append(apiGroup.ServerAddressByClientCIDRs, metav1.ServerAddressByClientCIDR{
		ClientCIDR:    "0.0.0.0/0",
		ServerAddress: "",
	})
	apiGroup.Kind = "APIGroup"
	return apiGroup
}

func (app *virtAPIApp) composeSubresources() {

	var subwss []*restful.WebService

	for _, version := range v1.SubresourceGroupVersions {
		subresourcesvmGVR := schema.GroupVersionResource{Group: version.Group, Version: version.Version, Resource: "virtualmachines"}
		subresourcesvmiGVR := schema.GroupVersionResource{Group: version.Group, Version: version.Version, Resource: "virtualmachineinstances"}

		subws := new(restful.WebService)
		subws.Doc(fmt.Sprintf("KubeVirt \"%s\" Subresource API.", version.Version))
		subws.Path(rest.GroupVersionBasePath(version))

		subresourceApp := rest.NewSubresourceAPIApp(app.virtCli, app.consoleServerPort)

		restartRouteBuilder := subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("restart")).
			To(subresourceApp.RestartVMRequestHandler).
			Reads(v1.RestartOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("restart").
			Doc("Restart a VirtualMachine object.").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusNotFound, "Not Found", nil).
			Returns(http.StatusBadRequest, "Bad Request", nil)
		restartRouteBuilder.ParameterNamed("body").Required(false)
		subws.Route(restartRouteBuilder)

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("migrate")).
			To(subresourceApp.MigrateVMRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("migrate").
			Doc("Migrate a running VirtualMachine to another node.").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusNotFound, "Not Found", nil).
			Returns(http.StatusBadRequest, "Bad Request", nil))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("start")).
			To(subresourceApp.StartVMRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("start").
			Doc("Start a VirtualMachine object.").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusNotFound, "Not Found", nil).
			Returns(http.StatusBadRequest, "Bad Request", nil))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("stop")).
			To(subresourceApp.StopVMRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("stop").
			Doc("Stop a VirtualMachine object.").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusNotFound, "Not Found", nil).
			Returns(http.StatusBadRequest, "Bad Request", nil))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("pause")).
			To(subresourceApp.PauseVMIRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("pause").
			Doc("Pause a VirtualMachineInstance object.").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusNotFound, "Not Found", nil).
			Returns(http.StatusBadRequest, "Bad Request", nil))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("unpause")).
			To(subresourceApp.UnpauseVMIRequestHandler). // handles VMIs as well
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("unpause").
			Doc("Unpause a VirtualMachineInstance object.").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusNotFound, "Not Found", nil).
			Returns(http.StatusBadRequest, "Bad Request", nil))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("console")).
			To(subresourceApp.ConsoleRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("console").
			Doc("Open a websocket connection to a serial console on the specified VirtualMachineInstance."))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("vnc")).
			To(subresourceApp.VNCRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("vnc").
			Doc("Open a websocket connection to connect to VNC on the specified VirtualMachineInstance."))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("test")).
			To(func(request *restful.Request, response *restful.Response) {
				response.WriteHeader(http.StatusOK)
			}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation("test").
			Doc("Test endpoint verifying apiserver connectivity."))

		subws.Route(subws.GET(rest.SubResourcePath("version")).Produces(restful.MIME_JSON).
			To(func(request *restful.Request, response *restful.Response) {
				response.WriteAsJson(virtversion.Get())
			}).Operation("version"))

		subws.Route(subws.GET(rest.SubResourcePath("healthz")).
			To(healthz.KubeConnectionHealthzFunc).
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Operation("checkHealth").
			Doc("Health endpoint").
			Returns(http.StatusOK, "OK", nil).
			Returns(http.StatusInternalServerError, "Unhealthy", nil))

		// Return empty api resource list.
		// K8s expects to be able to retrieve a resource list for each aggregated
		// app in order to discover what resources it provides. Without returning
		// an empty list here, there's a bug in the k8s resource discovery that
		// breaks kubectl's ability to reference short names for resources.
		subws.Route(subws.GET("/").
			Produces(restful.MIME_JSON).Writes(metav1.APIResourceList{}).
			To(func(request *restful.Request, response *restful.Response) {
				list := &metav1.APIResourceList{}

				list.Kind = "APIResourceList"
				list.GroupVersion = version.Group + "/" + version.Version
				list.APIVersion = version.Version
				list.APIResources = []metav1.APIResource{
					{
						Name:       "virtualmachineinstances/vnc",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/console",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/pause",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/unpause",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/start",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/stop",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/restart",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/migrate",
						Namespaced: true,
					},
				}

				response.WriteAsJson(list)
			}).
			Operation("getAPIResources").
			Doc("Get a KubeVirt API resources").
			Returns(http.StatusOK, "OK", metav1.APIResourceList{}).
			Returns(http.StatusNotFound, "Not Found", nil))

		restful.Add(subws)

		subwss = append(subwss, subws)
	}
	ws := new(restful.WebService)

	// K8s needs the ability to query the root paths
	ws.Route(ws.GET("/").
		Produces(restful.MIME_JSON).Writes(metav1.RootPaths{}).
		To(func(request *restful.Request, response *restful.Response) {
			paths := []string{"/apis",
				"/apis/",
				"/openapi/v2",
			}
			for _, version := range v1.SubresourceGroupVersions {
				paths = append(paths, rest.GroupBasePath(version))
				paths = append(paths, rest.GroupVersionBasePath(version))
			}
			response.WriteAsJson(&metav1.RootPaths{
				Paths: paths,
			})
		}).
		Operation("getRootPaths").
		Doc("Get KubeVirt API root paths").
		Returns(http.StatusOK, "OK", metav1.RootPaths{}).
		Returns(http.StatusNotFound, "Not Found", nil))

	for _, version := range v1.SubresourceGroupVersions {
		// K8s needs the ability to query info about a specific API group
		ws.Route(ws.GET(rest.GroupBasePath(version)).
			Produces(restful.MIME_JSON).Writes(metav1.APIGroup{}).
			To(func(request *restful.Request, response *restful.Response) {
				response.WriteAsJson(subresourceAPIGroup())
			}).
			Operation("getAPIGroup").
			Doc("Get a KubeVirt API Group").
			Returns(http.StatusOK, "OK", metav1.APIGroup{}).
			Returns(http.StatusNotFound, "Not Found", nil))
	}

	// K8s needs the ability to query the list of API groups this endpoint supports
	ws.Route(ws.GET("apis").
		Produces(restful.MIME_JSON).Writes(metav1.APIGroupList{}).
		To(func(request *restful.Request, response *restful.Response) {
			list := &metav1.APIGroupList{}
			list.Kind = "APIGroupList"
			list.Groups = append(list.Groups, subresourceAPIGroup())
			response.WriteAsJson(list)
		}).
		Operation("getAPIGroupList").
		Doc("Get a KubeVirt API GroupList").
		Returns(http.StatusOK, "OK", metav1.APIGroupList{}).
		Returns(http.StatusNotFound, "Not Found", nil))

	once := sync.Once{}
	var openapispec *spec.Swagger
	ws.Route(ws.GET("openapi/v2").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON).
		To(func(request *restful.Request, response *restful.Response) {
			once.Do(func() {
				openapispec = openapi.LoadOpenAPISpec([]*restful.WebService{ws, subwss[0]})
				openapispec.Info.Version = virtversion.Get().String()
			})
			response.WriteAsJson(openapispec)
		}))

	restful.Add(ws)
}

func (app *virtAPIApp) Compose() {

	app.composeSubresources()

	restful.Filter(filter.RequestLoggingFilter())
	restful.Filter(restful.OPTIONSFilter())
	restful.Filter(func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		allowed, reason, err := app.authorizor.Authorize(req)
		if err != nil {

			log.Log.Reason(err).Error("internal error during auth request")
			resp.WriteHeader(http.StatusInternalServerError)
			return
		} else if allowed {
			// request is permitted, so proceed with filter chain.
			chain.ProcessFilter(req, resp)
			return
		}
		resp.WriteErrorString(http.StatusUnauthorized, reason)
	})
}

func (app *virtAPIApp) ConfigureOpenAPIService() {
	restful.DefaultContainer.Add(restfulspec.NewOpenAPIService(openapi.CreateOpenAPIConfig(restful.RegisteredWebServices())))
	http.Handle("/swagger-ui/", http.StripPrefix("/swagger-ui/", http.FileServer(http.Dir(app.SwaggerUI))))
}

func deserializeStrings(in string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var ret []string
	if err := json.Unmarshal([]byte(in), &ret); err != nil {
		return nil, err
	}
	return ret, nil
}

func (app *virtAPIApp) readRequestHeader() error {
	authConfigMap, err := app.virtCli.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(util.ExtensionAPIServerAuthenticationConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// The request-header CA is mandatory. It can be retrieved from the configmap as we do here, or it must be provided
	// via flag on start of this apiserver. Since we don't do the latter, the former is mandatory for us
	// see https://github.com/kubernetes-incubator/apiserver-builder-alpha/blob/master/docs/concepts/auth.md#requestheader-authentication
	_, ok := authConfigMap.Data[util.RequestHeaderClientCAFileKey]
	if !ok {
		return fmt.Errorf("requestheader-client-ca-file not found in extension-apiserver-authentication ConfigMap")
	}

	// This config map also contains information about what
	// headers our authorizor should inspect
	headers, ok := authConfigMap.Data["requestheader-username-headers"]
	if ok {
		headerList, err := deserializeStrings(headers)
		if err != nil {
			return err
		}
		app.authorizor.AddUserHeaders(headerList)
	}

	headers, ok = authConfigMap.Data["requestheader-group-headers"]
	if ok {
		headerList, err := deserializeStrings(headers)
		if err != nil {
			return err
		}
		app.authorizor.AddGroupHeaders(headerList)
	}

	headers, ok = authConfigMap.Data["requestheader-extra-headers-prefix"]
	if ok {
		headerList, err := deserializeStrings(headers)
		if err != nil {
			return err
		}
		app.authorizor.AddExtraPrefixHeaders(headerList)
	}
	return nil
}

func (app *virtAPIApp) getSelfSignedCert() error {
	var ok bool

	caKeyPair, _ := triple.NewCA("kubevirt.io")
	keyPair, _ := triple.NewServerKeyPair(
		caKeyPair,
		"virt-api."+app.namespace+".pod.cluster.local",
		"virt-api",
		app.namespace,
		"cluster.local",
		nil,
		nil,
	)

	secret := &k8sv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      virtApiCertSecretName,
			Namespace: app.namespace,
			Labels: map[string]string{
				v1.AppLabel: "virt-api-aggregator",
			},
		},
		Type: "Opaque",
		Data: map[string][]byte{
			certBytesValue:        cert.EncodeCertPEM(keyPair.Cert),
			keyBytesValue:         cert.EncodePrivateKeyPEM(keyPair.Key),
			signingCertBytesValue: cert.EncodeCertPEM(caKeyPair.Cert),
		},
	}
	_, err := app.virtCli.CoreV1().Secrets(app.namespace).Create(secret)
	if errors.IsAlreadyExists(err) {
		secret, err = app.virtCli.CoreV1().Secrets(app.namespace).Get(virtApiCertSecretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// retrieve self signed cert info from secret

	app.certBytes, ok = secret.Data[certBytesValue]
	if !ok {
		return fmt.Errorf("%s value not found in %s virt-api secret", certBytesValue, virtApiCertSecretName)
	}
	app.keyBytes, ok = secret.Data[keyBytesValue]
	if !ok {
		return fmt.Errorf("%s value not found in %s virt-api secret", keyBytesValue, virtApiCertSecretName)
	}
	app.signingCertBytes, ok = secret.Data[signingCertBytesValue]
	if !ok {
		return fmt.Errorf("%s value not found in %s virt-api secret", signingCertBytesValue, virtApiCertSecretName)
	}
	return nil
}

func (app *virtAPIApp) createWebhook() error {
	err := app.createValidatingWebhook()
	if err != nil {
		return err
	}
	err = app.createMutatingWebhook()
	if err != nil {
		return err
	}
	return nil
}

func (app *virtAPIApp) validatingWebhooks() []admissionregistrationv1beta1.ValidatingWebhook {

	vmiPathCreate := vmiCreateValidatePath
	vmiPathUpdate := vmiUpdateValidatePath
	vmPath := vmValidatePath
	vmirsPath := vmirsValidatePath
	vmipresetPath := vmipresetValidatePath
	migrationCreatePath := migrationCreateValidatePath
	migrationUpdatePath := migrationUpdateValidatePath
	failurePolicy := admissionregistrationv1beta1.Fail

	webHooks := []admissionregistrationv1beta1.ValidatingWebhook{
		{
			Name:          "virtualmachineinstances-create-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstances"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmiPathCreate,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name:          "virtualmachineinstances-update-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstances"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmiPathUpdate,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name:          "virtualmachine-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachines"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmPath,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name:          "virtualmachinereplicaset-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstancereplicasets"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmirsPath,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name:          "virtualmachinepreset-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstancepresets"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmipresetPath,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name:          "migration-create-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstancemigrations"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &migrationCreatePath,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name:          "migration-update-validator.kubevirt.io",
			FailurePolicy: &failurePolicy,
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstancemigrations"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &migrationUpdatePath,
				},
				CABundle: app.signingCertBytes,
			},
		},
	}

	return webHooks
}

func (app *virtAPIApp) createValidatingWebhook() error {
	registerWebhook := false
	webhookRegistration, err := app.virtCli.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Get(virtWebhookValidator, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			fmt.Println(err)
			registerWebhook = true
		} else {
			return err
		}
	}

	webHooks := app.validatingWebhooks()

	if registerWebhook {
		_, err := app.virtCli.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Create(&admissionregistrationv1beta1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: virtWebhookValidator,
				Labels: map[string]string{
					v1.AppLabel:       virtWebhookValidator,
					v1.ManagedByLabel: v1.ManagedByLabelOperatorValue,
				},
			},
			Webhooks: webHooks,
		})
		if err != nil {
			return err
		}
	} else {

		// Ensure that we have the operator label attached, so that the operator can manage the resource later
		// This is part of a soft transition from behing controlled by the apiserver, to being controlled by the operator
		webhookRegistration.Labels[v1.ManagedByLabel] = v1.ManagedByLabelOperatorValue

		// update registered webhook with our data
		webhookRegistration.Webhooks = webHooks

		_, err := app.virtCli.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations().Update(webhookRegistration)
		if err != nil {
			return err
		}
	}

	http.HandleFunc(vmiCreateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMICreate(w, r, app.clusterConfig)
	})
	http.HandleFunc(vmiUpdateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMIUpdate(w, r)
	})
	http.HandleFunc(vmValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMs(w, r, app.clusterConfig, app.virtCli)
	})
	http.HandleFunc(vmirsValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMIRS(w, r, app.clusterConfig)
	})
	http.HandleFunc(vmipresetValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMIPreset(w, r)
	})
	http.HandleFunc(migrationCreateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeMigrationCreate(w, r, app.clusterConfig)
	})
	http.HandleFunc(migrationUpdateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeMigrationUpdate(w, r)
	})

	return nil
}

func (app *virtAPIApp) mutatingWebhooks() []admissionregistrationv1beta1.MutatingWebhook {
	vmPath := vmMutatePath
	vmiPath := vmiMutatePath
	migrationPath := migrationMutatePath
	webHooks := []admissionregistrationv1beta1.MutatingWebhook{
		{
			Name: "virtualmachines-mutator.kubevirt.io",
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
					admissionregistrationv1beta1.Update,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachines"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmPath,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name: "virtualmachineinstances-mutator.kubevirt.io",
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstances"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &vmiPath,
				},
				CABundle: app.signingCertBytes,
			},
		},
		{
			Name: "migrations-mutator.kubevirt.io",
			Rules: []admissionregistrationv1beta1.RuleWithOperations{{
				Operations: []admissionregistrationv1beta1.OperationType{
					admissionregistrationv1beta1.Create,
				},
				Rule: admissionregistrationv1beta1.Rule{
					APIGroups:   []string{v1.GroupName},
					APIVersions: v1.ApiSupportedWebhookVersions,
					Resources:   []string{"virtualmachineinstancemigrations"},
				},
			}},
			ClientConfig: admissionregistrationv1beta1.WebhookClientConfig{
				Service: &admissionregistrationv1beta1.ServiceReference{
					Namespace: app.namespace,
					Name:      virtApiServiceName,
					Path:      &migrationPath,
				},
				CABundle: app.signingCertBytes,
			},
		},
	}
	return webHooks
}

func (app *virtAPIApp) createMutatingWebhook() error {
	registerWebhook := false

	webhookRegistration, err := app.virtCli.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Get(virtWebhookMutator, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			registerWebhook = true
		} else {
			return err
		}
	}

	webHooks := app.mutatingWebhooks()

	if registerWebhook {
		_, err := app.virtCli.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Create(&admissionregistrationv1beta1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: virtWebhookMutator,
				Labels: map[string]string{
					v1.AppLabel:       virtWebhookMutator,
					v1.ManagedByLabel: v1.ManagedByLabelOperatorValue,
				},
			},
			Webhooks: webHooks,
		})
		if err != nil {
			return err
		}
	} else {
		// Ensure that we have the operator label attached, so that the operator can manage the resource later
		// This is part of a soft transition from behing controlled by the apiserver, to being controlled by the operator
		webhookRegistration.Labels[v1.ManagedByLabel] = v1.ManagedByLabelOperatorValue

		// update registered webhook with our data
		webhookRegistration.Webhooks = webHooks

		_, err := app.virtCli.AdmissionregistrationV1beta1().MutatingWebhookConfigurations().Update(webhookRegistration)
		if err != nil {
			return err
		}
	}

	http.HandleFunc(vmMutatePath, func(w http.ResponseWriter, r *http.Request) {
		mutating_webhook.ServeVMs(w, r, app.clusterConfig)
	})
	http.HandleFunc(vmiMutatePath, func(w http.ResponseWriter, r *http.Request) {
		mutating_webhook.ServeVMIs(w, r, app.clusterConfig)
	})
	http.HandleFunc(migrationMutatePath, func(w http.ResponseWriter, r *http.Request) {
		mutating_webhook.ServeMigrationCreate(w, r)
	})
	return nil
}

func (app *virtAPIApp) subresourceApiservice(version schema.GroupVersion) *apiregistrationv1beta1.APIService {

	subresourceAggregatedApiName := version.Version + "." + version.Group

	return &apiregistrationv1beta1.APIService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      subresourceAggregatedApiName,
			Namespace: app.namespace,
			Labels: map[string]string{
				v1.AppLabel:       "virt-api-aggregator",
				v1.ManagedByLabel: v1.ManagedByLabelOperatorValue,
			},
		},
		Spec: apiregistrationv1beta1.APIServiceSpec{
			Service: &apiregistrationv1beta1.ServiceReference{
				Namespace: app.namespace,
				Name:      virtApiServiceName,
			},
			Group:                version.Group,
			Version:              version.Version,
			CABundle:             app.signingCertBytes,
			GroupPriorityMinimum: 1000,
			VersionPriority:      15,
		},
	}
}

func (app *virtAPIApp) createSubresourceApiservice(version schema.GroupVersion) error {

	subresourceApiservice := app.subresourceApiservice(version)

	registerApiService := false

	apiService, err := app.aggregatorClient.ApiregistrationV1beta1().APIServices().Get(subresourceApiservice.Name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			registerApiService = true
		} else {
			return err
		}
	}

	if registerApiService {
		_, err = app.aggregatorClient.ApiregistrationV1beta1().APIServices().Create(app.subresourceApiservice(version))
		if err != nil {
			return err
		}
	} else {

		// Ensure that we have the operator label attached, so that the operator can manage the resource later
		// This is part of a soft transition from behing controlled by the apiserver, to being controlled by the operator
		apiService.Labels[v1.ManagedByLabel] = v1.ManagedByLabelOperatorValue

		// Always update spec to latest.
		apiService.Spec = app.subresourceApiservice(version).Spec
		_, err := app.aggregatorClient.ApiregistrationV1beta1().APIServices().Update(apiService)
		if err != nil {
			return err
		}
	}
	return nil
}

func (app *virtAPIApp) setupTLS(caManager webhooksutils.ClientCAManager) error {

	certPair, err := tls.X509KeyPair(app.certBytes, app.keyBytes)
	if err != nil {
		return fmt.Errorf("some special error: %b", err)
	}
	// A VerifyClientCertIfGiven request means we're not guaranteed
	// a client has been authenticated unless they provide a peer
	// cert.
	//
	// Make sure to verify in subresource endpoint that peer cert
	// was provided before processing request. If the peer cert is
	// given on the connection, then we can be guaranteed that it
	// was signed by the client CA in our pool.
	//
	// There is another ClientAuth type called 'RequireAndVerifyClientCert'
	// We can't use this type here because during the aggregated api status
	// check it attempts to hit '/' on our api endpoint to verify an http
	// response is given. That status request won't send a peer cert regardless
	// if the TLS handshake requests it. As a result, the TLS handshake fails
	// and our aggregated endpoint never becomes available.
	app.tlsConfig = webhooksutils.SetupTLS(caManager, certPair, tls.VerifyClientCertIfGiven)
	return nil
}

func (app *virtAPIApp) startTLS(stopCh <-chan struct{}) error {

	errors := make(chan error)

	informerFactory := controller.NewKubeInformerFactory(app.virtCli.RestClient(), app.virtCli, app.aggregatorClient, app.namespace)

	authConfigMapInformer := informerFactory.ApiAuthConfigMap()
	informerFactory.Start(stopCh)

	cache.WaitForCacheSync(stopCh, authConfigMapInformer.HasSynced)

	caManager := webhooksutils.NewClientCAManager(authConfigMapInformer.GetStore())

	err := app.setupTLS(caManager)
	if err != nil {
		return err
	}

	// start TLS server
	go func() {
		http.Handle("/metrics", promhttp.Handler())

		server := &http.Server{
			Addr:      fmt.Sprintf("%s:%d", app.BindAddress, app.Port),
			TLSConfig: app.tlsConfig,
		}

		errors <- server.ListenAndServeTLS("", "")
	}()

	// wait for server to exit
	return <-errors
}

func (app *virtAPIApp) Run() {
	// get client Cert
	err := app.readRequestHeader()
	if err != nil {
		panic(err)
	}

	// Get/Set selfsigned cert
	err = app.getSelfSignedCert()
	if err != nil {
		panic(err)
	}

	// Verify/create aggregator endpoint.
	for _, version := range v1.SubresourceGroupVersions {
		err = app.createSubresourceApiservice(version)
		if err != nil {
			panic(err)
		}
	}

	// Run informers for webhooks usage
	webhookInformers := webhooks.GetInformers()
	kubeInformerFactory := controller.NewKubeInformerFactory(app.virtCli.RestClient(), app.virtCli, app.aggregatorClient, app.namespace)
	configMapInformer := kubeInformerFactory.ConfigMap()
	crdInformer := kubeInformerFactory.CRD()

	stopChan := make(chan struct{}, 1)
	defer close(stopChan)
	go webhookInformers.VMIInformer.Run(stopChan)
	go webhookInformers.VMIPresetInformer.Run(stopChan)
	go webhookInformers.NamespaceLimitsInformer.Run(stopChan)
	go configMapInformer.Run(stopChan)
	go crdInformer.Run(stopChan)
	cache.WaitForCacheSync(stopChan,
		webhookInformers.VMIInformer.HasSynced,
		webhookInformers.VMIPresetInformer.HasSynced,
		webhookInformers.NamespaceLimitsInformer.HasSynced,
		configMapInformer.HasSynced)

	app.clusterConfig = virtconfig.NewClusterConfig(configMapInformer, crdInformer, app.namespace)

	// Verify/create webhook endpoint.
	err = app.createWebhook()
	if err != nil {
		panic(err)
	}

	// start TLS server
	err = app.startTLS(stopChan)
	if err != nil {
		panic(err)
	}
}

func (app *virtAPIApp) AddFlags() {
	app.InitFlags()

	app.AddCommonFlags()

	flag.StringVar(&app.SwaggerUI, "swagger-ui", "third_party/swagger-ui",
		"swagger-ui location")
	flag.BoolVar(&app.SubresourcesOnly, "subresources-only", false,
		"Only serve subresource endpoints")
	flag.IntVar(&app.consoleServerPort, "console-server-port", defaultConsoleServerPort,
		"The port virt-handler listens on for console requests")
}