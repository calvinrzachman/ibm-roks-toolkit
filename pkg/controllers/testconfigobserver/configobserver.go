package testconfigobserver

// This is a copy of github.com/openshift/library-go/pkg/operator/configobserver/config_observer_controller.go
// with some modifications for unit testing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/imdario/mergo"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	originalconfigobserver "github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

type ConfigObserver struct {
	// observers are called in an undefined order and their results are merged to
	// determine the observed configuration.
	observers []originalconfigobserver.ObserveConfigFunc

	operatorClient v1helpers.OperatorClient

	// listers are used by config observers to retrieve necessary resources
	listers originalconfigobserver.Listers

	nestedConfigPath      []string
	degradedConditionType string
}

// NewConfigObserver creates a config observer that watches changes to a nested field (nestedConfigPath) in the config.
// Useful when the config is shared across multiple controllers in the same process.
//
// Example:
//
// Given the following configuration, you could run two separate controllers and point each to its own section.
// The first controller would be responsible for "oauthAPIServer" and the second for "oauthServer" section.
//
//	"observedConfig": {
//	  "oauthAPIServer": {
//	    "apiServerArguments": {"tls-min-version": "VersionTLS12"}
//	  },
//	  "oauthServer": {
//	    "corsAllowedOrigins": [ "//127\\.0\\.0\\.1(:|$)","//localhost(:|$)"]
//	  }
//	}
//
// oauthAPIController    := NewNestedConfigObserver(..., []string{"oauthAPIServer"}
// oauthServerController := NewNestedConfigObserver(..., []string{"oauthServer"}
func NewConfigObserver(
	operatorClient v1helpers.OperatorClient,
	listers originalconfigobserver.Listers,
	informers []factory.Informer,
	observers ...originalconfigobserver.ObserveConfigFunc,
) *ConfigObserver {
	c := &ConfigObserver{
		operatorClient:        operatorClient,
		observers:             observers,
		listers:               listers,
		nestedConfigPath:      []string{},
		degradedConditionType: condition.ConfigObservationDegradedConditionType,
	}
	return c
}

// sync reacts to a change in prereqs by finding information that is required to match another value in the cluster. This
// must be information that is logically "owned" by another component.
func (c ConfigObserver) Sync(ctx context.Context, syncCtx factory.SyncContext) error {
	originalSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	spec := originalSpec.DeepCopy()

	// don't worry about errors.  If we can't decode, we'll simply stomp over the field.
	existingConfig := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(spec.ObservedConfig.Raw)).Decode(&existingConfig); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}

	var errs []error
	var observedConfigs []map[string]interface{}
	for _, i := range rand.Perm(len(c.observers)) {
		var currErrs []error
		observedConfig, currErrs := c.observers[i](c.listers, syncCtx.Recorder(), existingConfig)
		observedConfigs = append(observedConfigs, observedConfig)
		errs = append(errs, currErrs...)
	}

	mergedObservedConfig := map[string]interface{}{}
	for _, observedConfig := range observedConfigs {
		if err := mergo.Merge(&mergedObservedConfig, observedConfig); err != nil {
			klog.Warningf("merging observed config failed: %v", err)
		}
	}

	reverseMergedObservedConfig := map[string]interface{}{}
	for i := len(observedConfigs) - 1; i >= 0; i-- {
		if err := mergo.Merge(&reverseMergedObservedConfig, observedConfigs[i]); err != nil {
			klog.Warningf("merging observed config failed: %v", err)
		}
	}

	if !equality.Semantic.DeepEqual(mergedObservedConfig, reverseMergedObservedConfig) {
		errs = append(errs, errors.New("non-deterministic config observation detected"))
	}

	if err := c.updateObservedConfig(syncCtx, existingConfig, mergedObservedConfig); err != nil {
		errs = []error{err}
	}
	configError := v1helpers.NewMultiLineAggregate(errs)

	// update failing condition
	cond := operatorv1.OperatorCondition{
		Type:   c.degradedConditionType,
		Status: operatorv1.ConditionFalse,
	}
	if configError != nil {
		cond.Status = operatorv1.ConditionTrue
		cond.Reason = "Error"
		cond.Message = configError.Error()
	}
	if _, _, updateError := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(cond)); updateError != nil {
		return updateError
	}

	return configError
}

func (c ConfigObserver) updateObservedConfig(syncCtx factory.SyncContext, existingConfig map[string]interface{}, mergedObservedConfig map[string]interface{}) error {
	if len(c.nestedConfigPath) == 0 {
		if !equality.Semantic.DeepEqual(existingConfig, mergedObservedConfig) {
			syncCtx.Recorder().Eventf("ObservedConfigChanged", "Writing updated observed config: %v", diff.ObjectDiff(existingConfig, mergedObservedConfig))
			return c.updateConfig(syncCtx, mergedObservedConfig, v1helpers.UpdateObservedConfigFn)
		}
		return nil
	}

	existingConfigNested, _, err := unstructured.NestedMap(existingConfig, c.nestedConfigPath...)
	if err != nil {
		return fmt.Errorf("unable to extract the config under %v key, err %v", c.nestedConfigPath, err)
	}
	mergedObservedConfigNested, _, err := unstructured.NestedMap(mergedObservedConfig, c.nestedConfigPath...)
	if err != nil {
		return fmt.Errorf("unable to extract the merged config under %v, err %v", c.nestedConfigPath, err)
	}
	if !equality.Semantic.DeepEqual(existingConfigNested, mergedObservedConfigNested) {
		syncCtx.Recorder().Eventf("ObservedConfigChanged", "Writing updated section (%q) of observed config: %q", strings.Join(c.nestedConfigPath, "/"), diff.ObjectDiff(existingConfigNested, mergedObservedConfigNested))
		return c.updateConfig(syncCtx, mergedObservedConfigNested, c.updateNestedConfigHelper)
	}
	return nil
}

type updateObservedConfigFn func(config map[string]interface{}) v1helpers.UpdateOperatorSpecFunc

func (c ConfigObserver) updateConfig(syncCtx factory.SyncContext, updatedMaybeNestedConfig map[string]interface{}, updateConfigHelper updateObservedConfigFn) error {
	if _, _, err := v1helpers.UpdateSpec(context.TODO(), c.operatorClient, updateConfigHelper(updatedMaybeNestedConfig)); err != nil {
		// At this point we failed to write the updated config. If we are permanently broken, do not pile the errors from observers
		// but instead reset the errors and only report single error condition.
		syncCtx.Recorder().Warningf("ObservedConfigWriteError", "Failed to write observed config: %v", err)
		return fmt.Errorf("error writing updated observed config: %v", err)
	}
	return nil
}

// updateNestedConfigHelper returns a helper function for updating the nested config.
func (c ConfigObserver) updateNestedConfigHelper(updatedNestedConfig map[string]interface{}) v1helpers.UpdateOperatorSpecFunc {
	return func(currentSpec *operatorv1.OperatorSpec) error {
		existingConfig := map[string]interface{}{}
		if err := json.NewDecoder(bytes.NewBuffer(currentSpec.ObservedConfig.Raw)).Decode(&existingConfig); err != nil {
			klog.V(4).Infof("decode of existing config failed with error: %v", err)
		}
		if err := unstructured.SetNestedField(existingConfig, updatedNestedConfig, c.nestedConfigPath...); err != nil {
			return fmt.Errorf("unable to set the nested (%q) observed config: %v", strings.Join(c.nestedConfigPath, "/"), err)
		}
		currentSpec.ObservedConfig = runtime.RawExtension{Object: &unstructured.Unstructured{Object: existingConfig}}
		return nil
	}
}

// listersToInformer converts the Listers interface to informer with empty AddEventHandler as we only care about synced caches in the Run.
func listersToInformer(l originalconfigobserver.Listers) []factory.Informer {
	result := make([]factory.Informer, len(l.PreRunHasSynced()))
	for i := range l.PreRunHasSynced() {
		result[i] = &listerInformer{cacheSynced: l.PreRunHasSynced()[i]}
	}
	return result
}

type listerInformer struct {
	cacheSynced cache.InformerSynced
}

func (l *listerInformer) AddEventHandler(cache.ResourceEventHandler) {
	return
}

func (l *listerInformer) HasSynced() bool {
	return l.cacheSynced()
}

// WithPrefix adds a prefix to the path the input observer would otherwise observe into
func WithPrefix(observer originalconfigobserver.ObserveConfigFunc, prefix ...string) originalconfigobserver.ObserveConfigFunc {
	if len(prefix) == 0 {
		return observer
	}

	return func(listers originalconfigobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
		errs := []error{}

		nestedExistingConfig, _, err := unstructured.NestedMap(existingConfig, prefix...)
		if err != nil {
			errs = append(errs, err)
		}

		orig, observerErrs := observer(listers, recorder, nestedExistingConfig)
		errs = append(errs, observerErrs...)

		if orig == nil {
			return nil, errs
		}

		ret := map[string]interface{}{}
		if err := unstructured.SetNestedField(ret, orig, prefix...); err != nil {
			errs = append(errs, err)
		}
		return ret, errs

	}
}
