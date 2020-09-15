// ------------------------------------------------------------
// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
// ------------------------------------------------------------

package config

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dapr/dapr/pkg/logger"
	"github.com/dapr/dapr/pkg/proto/common/v1"
	operatorv1pb "github.com/dapr/dapr/pkg/proto/operator/v1"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	"github.com/pkg/errors"
	"google.golang.org/grpc/peer"
	yaml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	operatorCallTimeout = time.Second * 5
	operatorMaxRetries  = 100
	AllowAccess         = "allow"
	DenyAccess          = "deny"
	// AccessControlActionAllow defines the allow action for an operation
	AccessControlActionAllow = "allow"
	// AccessControlActionDeny defines the deny action for an operation
	AccessControlActionDeny = "deny"
)

type Configuration struct {
	Spec ConfigurationSpec `json:"spec" yaml:"spec"`
}

// AccessControlList is an in-memory access control list config for fast lookup
type AccessControlList struct {
	DefaultAction string
	PolicySpec    map[string]AppPolicySpec
}

type ConfigurationSpec struct {
	HTTPPipelineSpec PipelineSpec `json:"httpPipeline,omitempty" yaml:"httpPipeline,omitempty"`
	TracingSpec      TracingSpec  `json:"tracing,omitempty" yaml:"tracing,omitempty"`
	MTLSSpec         MTLSSpec     `json:"mtls,omitempty"`
	MetricSpec       MetricSpec   `json:"metric,omitempty" yaml:"metric,omitempty"`
	Secrets          SecretsSpec  `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	AccessControlSpec AccessControlSpec `json:"accessControl,omitempty" yaml:"accessControl,omitempty"`
}

type SecretsSpec struct {
	Scopes []SecretsScope `json:"scopes"`
}

// SecretsScope defines the scope for secrets
type SecretsScope struct {
	DefaultAccess  string   `json:"defaultAccess,omitempty" yaml:"defaultAccess,omitempty"`
	StoreName      string   `json:"storeName" yaml:"storeName"`
	AllowedSecrets []string `json:"allowedSecrets,omitempty" yaml:"allowedSecrets,omitempty"`
	DeniedSecrets  []string `json:"deniedSecrets,omitempty" yaml:"deniedSecrets,omitempty"`
}

type PipelineSpec struct {
	Handlers []HandlerSpec `json:"handlers" yaml:"handlers"`
}

type HandlerSpec struct {
	Name         string       `json:"name" yaml:"name"`
	Type         string       `json:"type" yaml:"type"`
	SelectorSpec SelectorSpec `json:"selector,omitempty" yaml:"selector,omitempty"`
}

type SelectorSpec struct {
	Fields []SelectorField `json:"fields" yaml:"fields"`
}

type SelectorField struct {
	Field string `json:"field" yaml:"field"`
	Value string `json:"value" yaml:"value"`
}

type TracingSpec struct {
	SamplingRate string `json:"samplingRate" yaml:"samplingRate"`
	Stdout       bool   `json:"stdout" yaml:"stdout"`
}

// MetricSpec configuration for metrics
type MetricSpec struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// AppPolicySpec defines the policy data structure for each app
type AppPolicySpec struct {
	AppName             string         `json:"app" yaml:"app"`
	DefaultAction       string         `json:"defaultAction" yaml:"defaultAction"`
	TrustDomain         string         `json:"trustDomain" yaml:"trustDomain"`
	AppOperationActions []AppOperation `json:"operations" yaml:"operations"`
}

// AppOperation defines the data structure for each app operation
type AppOperation struct {
	Operation string   `json:"name" yaml:"name"`
	HTTPVerb  []string `json:"httpVerb" yaml:"httpVerb"`
	Action    string   `json:"action" yaml:"action"`
}

// AccessControlSpec is the spec object in ConfigurationSpec
type AccessControlSpec struct {
	DefaultAction string          `json:"defaultAction" yaml:"defaultAction"`
	AppPolicies   []AppPolicySpec `json:"policies" yaml:"policies"`
}

type MTLSSpec struct {
	Enabled          bool   `json:"enabled"`
	WorkloadCertTTL  string `json:"workloadCertTTL"`
	AllowedClockSkew string `json:"allowedClockSkew"`
}

// SpiffeID represents the separated fields in a spiffe id
type SpiffeID struct {
	trustDomain string
	namespace   string
	appID       string
}

// LoadDefaultConfiguration returns the default config
func LoadDefaultConfiguration() *Configuration {
	return &Configuration{
		Spec: ConfigurationSpec{
			TracingSpec: TracingSpec{
				SamplingRate: "",
			},
			MetricSpec: MetricSpec{
				Enabled: true,
			},
		},
	}
}

// LoadStandaloneConfiguration gets the path to a config file and loads it into a configuration
func LoadStandaloneConfiguration(config string) (*Configuration, error) {
	_, err := os.Stat(config)
	if err != nil {
		return nil, err
	}

	b, err := ioutil.ReadFile(config)
	if err != nil {
		return nil, err
	}

	var conf Configuration
	err = yaml.Unmarshal(b, &conf)
	if err != nil {
		return nil, err
	}
	err = sortAndValidateSecretsConfiguration(&conf)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}

// LoadKubernetesConfiguration gets configuration from the Kubernetes operator with a given name
func LoadKubernetesConfiguration(config, namespace string, operatorClient operatorv1pb.OperatorClient) (*Configuration, error) {
	resp, err := operatorClient.GetConfiguration(context.Background(), &operatorv1pb.GetConfigurationRequest{
		Name:      config,
		Namespace: namespace,
	}, grpc_retry.WithMax(operatorMaxRetries), grpc_retry.WithPerRetryTimeout(operatorCallTimeout))
	if err != nil {
		return nil, err
	}
	if resp.GetConfiguration() == nil {
		return nil, errors.Errorf("configuration %s not found", config)
	}
	var conf Configuration
	err = json.Unmarshal(resp.GetConfiguration(), &conf)
	if err != nil {
		return nil, err
	}

	err = sortAndValidateSecretsConfiguration(&conf)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}

// Validate the secrets configuration and sort the allow and deny lists if present.
func sortAndValidateSecretsConfiguration(conf *Configuration) error {
	scopes := conf.Spec.Secrets.Scopes
	set := sets.NewString()
	for _, scope := range scopes {
		// validate scope
		if set.Has(scope.StoreName) {
			return errors.Errorf("%q storeName is repeated in secrets configuration", scope.StoreName)
		}
		if scope.DefaultAccess != "" &&
			!strings.EqualFold(scope.DefaultAccess, AllowAccess) &&
			!strings.EqualFold(scope.DefaultAccess, DenyAccess) {
			return errors.Errorf("defaultAccess %q can be either allow or deny", scope.DefaultAccess)
		}
		set.Insert(scope.StoreName)

		// modify scope
		sort.Strings(scope.AllowedSecrets)
		sort.Strings(scope.DeniedSecrets)
	}

	return nil
}

// Check if the secret is allowed to be accessed.
func (c SecretsScope) IsSecretAllowed(key string) bool {
	// By default set allow access for the secret store.
	var access string = AllowAccess
	// Check and set deny access.
	if strings.EqualFold(c.DefaultAccess, DenyAccess) {
		access = DenyAccess
	}

	// If the allowedSecrets list is not empty then check if the access is specifically allowed for this key.
	if len(c.AllowedSecrets) != 0 {
		return containsKey(c.AllowedSecrets, key)
	}

	// Check key in deny list if deny list is present for the secret store.
	// If the specific key is denied, then alone deny access.
	if deny := containsKey(c.DeniedSecrets, key); deny {
		return !deny
	}

	// Check if defined default access is allow.
	return access == AllowAccess
}

// Runs Binary Search on a sorted list of strings to find a key.
func containsKey(s []string, key string) bool {
	index := sort.SearchStrings(s, key)

	return index < len(s) && s[index] == key
}

// TranslateAccessControlSpec creates an in-memory copy of the Access Control Spec for fast lookup
func TranslateAccessControlSpec(accessControlSpec AccessControlSpec, id string) AccessControlList {
	var accessControlList AccessControlList
	accessControlList.PolicySpec = make(map[string]AppPolicySpec)
	accessControlList.DefaultAction = strings.ToLower(accessControlSpec.DefaultAction)
	var log = logger.NewLogger("dapr.configuration")
	log.Infof("@@@@@ Translating policy spec....")

	for _, appPolicySpec := range accessControlSpec.AppPolicies {
		log.Infof("@@@@@ name: %s spec: %s", appPolicySpec.AppName, appPolicySpec)
		accessControlList.PolicySpec[appPolicySpec.AppName] = appPolicySpec
	}

	return accessControlList
}

// TryGetAndParseSpiffeID retrieves the SPIFFE Id from the cert and parses it
func TryGetAndParseSpiffeID(ctx context.Context) (*SpiffeID, error) {
	peer, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("could not retrieve spiffe id from the grpc context")
	}

	fmt.Println(peer)

	// if peer.AuthInfo == nil {
	// 	return nil, fmt.Errorf("could not retrieve auth info from grpc context tls info")
	// }

	// tlsInfo := peer.AuthInfo.(credentials.TLSInfo)

	// if tlsInfo.State.HandshakeComplete == false {
	// 	return nil, fmt.Errorf("tls handshake is not complete")
	// }

	// certChain := tlsInfo.State.VerifiedChains
	// t := reflect.TypeOf(certChain)
	// fmt.Println(t)
	// if certChain == nil || len(certChain[0]) == 0 {
	// 	return nil, fmt.Errorf("could not retrieve read client cert info")
	// }

	// TODO: Remove hardcoding for testing
	// spiffeID := string(certChain[0][0].ExtraExtensions[0].Value)
	spiffeID := "spiffe://a/ns/b/pythonapp"
	fmt.Printf("spiffe id :- %v\n", spiffeID)

	// The SPIFFE Id will be of the format: spiffe://<trust-domain/ns/<namespace>/<app-id>
	parts := strings.Split(spiffeID, "/")
	var id SpiffeID
	id.trustDomain = parts[2]
	id.namespace = parts[4]
	id.appID = parts[5]

	return &id, nil
}

// IsOperationAllowedByAccessControlPolicy determines if access control policies allow the operation on the target app
func IsOperationAllowedByAccessControlPolicy(id *SpiffeID, srcAppID string, operation string, httpVerb common.HTTPExtension_Verb, accessControlList *AccessControlList) bool {
	var log = logger.NewLogger("dapr.configuration")
	log.Infof("@@@@ Dumping all policy specs....")
	for key, spec := range accessControlList.PolicySpec {
		log.Infof("key: %s, value: %s", key, spec)
	}
	log.Infof("Checking access control policy for invocation by %v, operation: %v, httpVerb: %v", srcAppID, operation, httpVerb)
	action := accessControlList.DefaultAction

	if accessControlList == nil {
		// No access control list is provided. Do nothing
		return true
	}

	policy, found := accessControlList.PolicySpec[srcAppID]
	log.Infof("@@@@ Using policy spec: %v", policy)

	if !found {
		return isActionAllowed(action)
	}

	action = policy.DefaultAction

	if id == nil {
		log.Errorf("Unable to verify spiffe id of the client. Will apply default access control policy")
	} else {
		if policy.TrustDomain != "*" && policy.TrustDomain != id.trustDomain {
			log.Infof("Trust Domain mismatch does not allow request")
			return false
		}

		// TODO: Check namespace if needed

		inputOperation := "/" + operation
		// Check the operation specific policy
		for _, policyOperation := range policy.AppOperationActions {
			if strings.HasPrefix(policyOperation.Operation, inputOperation) {
				log.Infof("Found operation: %v. checking http verbs", inputOperation)
				if httpVerb != common.HTTPExtension_NONE {
					for _, policyVerb := range policyOperation.HTTPVerb {
						if policyVerb == httpVerb.String() || policyVerb == "*" {
							action = policyOperation.Action
							log.Infof("Applying action: %v for srcAppId: %s operation: %v, verb: %v", srcAppID, action, inputOperation, policyVerb)
							break
						}
					}
				} else {
					log.Infof("Applying action: %v for operation: %v", action, inputOperation)
					action = policyOperation.Action
				}
			}
		}
	}

	log.Infof("Applying access control policy action: %v", action)
	return isActionAllowed(action)
}

func isActionAllowed(action string) bool {
	if strings.ToLower(action) == AccessControlActionAllow {
		return true
	}
	return false
}
