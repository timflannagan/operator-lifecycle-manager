package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blang/semver/v4"
	kuberpakv1alpha1 "github.com/joelanford/kuberpak/api/v1alpha1"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	v1alpha1listers "github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/listers/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry/resolver/cache"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry/resolver/projection"
	"github.com/operator-framework/operator-registry/pkg/api"
	opregistry "github.com/operator-framework/operator-registry/pkg/registry"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type csvSourceProvider struct {
	csvLister v1alpha1listers.ClusterServiceVersionLister
	subLister v1alpha1listers.SubscriptionLister
	logger    logrus.StdLogger
	client    client.Client
}

func NewCSVSourceProvider(
	csvLister v1alpha1listers.ClusterServiceVersionLister,
	subLister v1alpha1listers.SubscriptionLister,
	logger logrus.FieldLogger,
	client client.Client,
) *csvSourceProvider {
	return &csvSourceProvider{
		csvLister: csvLister,
		subLister: subLister,
		logger:    logger,
		client:    client,
	}
}

func (csp *csvSourceProvider) Sources(namespaces ...string) map[cache.SourceKey]cache.Source {
	result := make(map[cache.SourceKey]cache.Source)
	for _, namespace := range namespaces {
		result[cache.NewVirtualSourceKey(namespace)] = &csvSource{
			key:       cache.NewVirtualSourceKey(namespace),
			csvLister: csp.csvLister.ClusterServiceVersions(namespace),
			subLister: csp.subLister.Subscriptions(namespace),
			logger:    csp.logger,
			client:    csp.client,
		}
	}
	return result
}

type csvSource struct {
	key       cache.SourceKey
	csvLister v1alpha1listers.ClusterServiceVersionNamespaceLister
	subLister v1alpha1listers.SubscriptionNamespaceLister
	client    client.Client
	logger    logrus.StdLogger
}

func (s *csvSource) Snapshot(ctx context.Context) (*cache.Snapshot, error) {
	csvs, err := s.csvLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	subs, err := s.subLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	// build a catalog snapshot of CSVs without subscriptions
	csvSubscriptions := make(map[*v1alpha1.ClusterServiceVersion]*v1alpha1.Subscription)
	for _, sub := range subs {
		for _, csv := range csvs {
			if csv.Name == sub.Status.InstalledCSV {
				csvSubscriptions[csv] = sub
				break
			}
		}
	}

	csvBundleInstances := make(map[*v1alpha1.ClusterServiceVersion]*kuberpakv1alpha1.BundleInstance)
	bis := &kuberpakv1alpha1.BundleInstanceList{}
	if err := s.client.List(ctx, bis); err != nil {
		return nil, fmt.Errorf("failed to list the bundleinstances in the cluster: %w", err)
	}
	for _, bi := range bis.Items {
		bundle := &kuberpakv1alpha1.Bundle{}
		if err := s.client.Get(ctx, types.NamespacedName{Name: bi.Spec.BundleName}, bundle); err != nil {
			return nil, fmt.Errorf("failed to find the %s bundle referenced by the %s bundleinstance: %v", bi.Spec.BundleName, bi.GetName(), err)
		}
		for _, csv := range csvs {
			for _, obj := range bundle.Status.Info.Objects {
				if obj.Kind != "ClusterServiceVersion" {
					continue
				}
				if obj.Name != csv.Name {
					continue
				}
				s.logger.Printf("found the %s BI that specifies the %s bundle that specifies the %s CSV", bi.GetName(), bi.Spec.BundleName, csv.GetName())
				csvBundleInstances[csv] = &bi
				break
			}
		}
	}

	// TODO(tflannag): Filter out CSVs without BundleInstances
	// - Need a way to identify which BundleInstance is monitoring this namespace (e.g. metadata.Name, metadata.Annotation, $something_else)
	// - Investigate that BundleInstance's spec.Bundle
	// - Query for that Bundle and check whether the .status.info.objects matches the CSV (need to verify GVK as well)

	var csvsMissingProperties []*v1alpha1.ClusterServiceVersion
	var entries []*cache.Entry
	for _, csv := range csvs {
		if csv.IsCopied() {
			continue
		}
		entry, err := newEntryFromV1Alpha1CSV(csv)
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)

		if anno, ok := csv.GetAnnotations()[projection.PropertiesAnnotationKey]; !ok {
			csvsMissingProperties = append(csvsMissingProperties, csv)
			if inferred, err := s.inferProperties(csv, subs); err != nil {
				s.logger.Printf("unable to infer properties for csv %q: %w", csv.Name, err)
			} else {
				entry.Properties = append(entry.Properties, inferred...)
			}
		} else if props, err := projection.PropertyListFromPropertiesAnnotation(anno); err != nil {
			return nil, fmt.Errorf("failed to retrieve properties of csv %q: %w", csv.GetName(), err)
		} else {
			entry.Properties = props
		}

		entry.SourceInfo = &cache.OperatorSourceInfo{
			Catalog: s.key,
			// TODO: we may need to add guardrails around this
			Subscription: csvSubscriptions[csv],
		}
		// Try to determine source package name from properties and add to SourceInfo.
		for _, p := range entry.Properties {
			if p.Type != opregistry.PackageType {
				continue
			}
			var pp opregistry.PackageProperty
			err := json.Unmarshal([]byte(p.Value), &pp)
			if err != nil {
				s.logger.Printf("failed to unmarshal package property of csv %q: %w", csv.Name, err)
				continue
			}
			entry.SourceInfo.Package = pp.PackageName
		}
	}

	if len(csvsMissingProperties) > 0 {
		names := make([]string, len(csvsMissingProperties))
		for i, csv := range csvsMissingProperties {
			names[i] = csv.GetName()
		}
		s.logger.Printf("considered csvs without properties annotation during resolution: %v", names)
	}

	return &cache.Snapshot{Entries: entries}, nil
}

func (s *csvSource) inferProperties(csv *v1alpha1.ClusterServiceVersion, subs []*v1alpha1.Subscription) ([]*api.Property, error) {
	var properties []*api.Property

	packages := make(map[string]struct{})
	for _, sub := range subs {
		if sub.Status.InstalledCSV != csv.Name {
			continue
		}
		// // Without sanity checking the Subscription spec's
		// // package against catalog contents, updates to the
		// // Subscription spec could result in a bad package
		// // inference.
		// for _, entry := range s.cache.Namespaced(sub.Spec.CatalogSourceNamespace).Catalog(cache.SourceKey{Namespace: sub.Spec.CatalogSourceNamespace, Name: sub.Spec.CatalogSource}).Find(cache.And(cache.CSVNamePredicate(csv.Name), cache.PkgPredicate(sub.Spec.Package))) {
		// 	if pkg := entry.Package(); pkg != "" {
		// 		packages[pkg] = struct{}{}
		// 	}
		// }
		// todo
	}
	if l := len(packages); l != 1 {
		s.logger.Printf("could not unambiguously infer package name for %q (found %d distinct package names)", csv.Name, l)
		return properties, nil
	}
	var pkg string
	for pkg = range packages {
		// Assign the single key to pkg.
	}
	var version string // Emit empty string rather than "0.0.0" if .spec.version is zero-valued.
	if !csv.Spec.Version.Version.Equals(semver.Version{}) {
		version = csv.Spec.Version.String()
	}
	pp, err := json.Marshal(opregistry.PackageProperty{
		PackageName: pkg,
		Version:     version,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal inferred package property: %w", err)
	}
	properties = append(properties, &api.Property{
		Type:  opregistry.PackageType,
		Value: string(pp),
	})

	return properties, nil
}

func newEntryFromV1Alpha1CSV(csv *v1alpha1.ClusterServiceVersion) (*cache.Entry, error) {
	providedAPIs := cache.EmptyAPISet()
	for _, crdDef := range csv.Spec.CustomResourceDefinitions.Owned {
		parts := strings.SplitN(crdDef.Name, ".", 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("error parsing crd name: %s", crdDef.Name)
		}
		providedAPIs[opregistry.APIKey{Plural: parts[0], Group: parts[1], Version: crdDef.Version, Kind: crdDef.Kind}] = struct{}{}
	}
	for _, api := range csv.Spec.APIServiceDefinitions.Owned {
		providedAPIs[opregistry.APIKey{Group: api.Group, Version: api.Version, Kind: api.Kind, Plural: api.Name}] = struct{}{}
	}

	requiredAPIs := cache.EmptyAPISet()
	for _, crdDef := range csv.Spec.CustomResourceDefinitions.Required {
		parts := strings.SplitN(crdDef.Name, ".", 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("error parsing crd name: %s", crdDef.Name)
		}
		requiredAPIs[opregistry.APIKey{Plural: parts[0], Group: parts[1], Version: crdDef.Version, Kind: crdDef.Kind}] = struct{}{}
	}
	for _, api := range csv.Spec.APIServiceDefinitions.Required {
		requiredAPIs[opregistry.APIKey{Group: api.Group, Version: api.Version, Kind: api.Kind, Plural: api.Name}] = struct{}{}
	}

	properties, err := providedAPIsToProperties(providedAPIs)
	if err != nil {
		return nil, err
	}
	dependencies, err := requiredAPIsToProperties(requiredAPIs)
	if err != nil {
		return nil, err
	}
	properties = append(properties, dependencies...)

	return &cache.Entry{
		Name:         csv.GetName(),
		Version:      &csv.Spec.Version.Version,
		ProvidedAPIs: providedAPIs,
		RequiredAPIs: requiredAPIs,
		SourceInfo:   &cache.ExistingOperator,
		Properties:   properties,
	}, nil
}
