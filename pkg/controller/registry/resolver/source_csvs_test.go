package resolver

import (
	"testing"

	"github.com/blang/semver/v4"
	opver "github.com/operator-framework/api/pkg/lib/version"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry/resolver/cache"
	"github.com/operator-framework/operator-registry/pkg/api"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInferProperties(t *testing.T) {
	catalog := cache.SourceKey{Namespace: "namespace", Name: "name"}

	for _, tc := range []struct {
		Name          string
		Cache         cache.StaticSourceProvider
		CSV           *v1alpha1.ClusterServiceVersion
		Subscriptions []*v1alpha1.Subscription
		Expected      []*api.Property
	}{
		{
			Name: "no subscriptions infers no properties",
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
			},
		},
		{
			Name: "one unrelated subscription infers no properties",
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package: "x",
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "b",
					},
				},
			},
		},
		{
			Name: "one subscription with empty package field infers no properties",
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package: "",
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
			},
		},
		{
			Name: "two related subscriptions infers no properties",
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package: "x",
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package: "x",
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
			},
		},
		{
			Name: "one matching subscription infers package property",
			Cache: cache.StaticSourceProvider{
				catalog: &cache.Snapshot{
					Entries: []*cache.Entry{
						{
							Name: "a",
							SourceInfo: &cache.OperatorSourceInfo{
								Package: "x",
							},
						},
					},
				},
			},
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
				Spec: v1alpha1.ClusterServiceVersionSpec{
					Version: opver.OperatorVersion{Version: semver.MustParse("1.2.3")},
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package:                "x",
						CatalogSource:          catalog.Name,
						CatalogSourceNamespace: catalog.Namespace,
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
			},
			Expected: []*api.Property{
				{
					Type:  "olm.package",
					Value: `{"packageName":"x","version":"1.2.3"}`,
				},
			},
		},
		{
			Name: "one matching subscription to other-namespace catalogsource infers package property",
			Cache: cache.StaticSourceProvider{
				{Namespace: "other-namespace", Name: "other-name"}: &cache.Snapshot{
					Entries: []*cache.Entry{
						{
							Name: "a",
							SourceInfo: &cache.OperatorSourceInfo{
								Package: "x",
							},
						},
					},
				},
			},
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
				Spec: v1alpha1.ClusterServiceVersionSpec{
					Version: opver.OperatorVersion{Version: semver.MustParse("1.2.3")},
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package:                "x",
						CatalogSource:          "other-name",
						CatalogSourceNamespace: "other-namespace",
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
			},
			Expected: []*api.Property{
				{
					Type:  "olm.package",
					Value: `{"packageName":"x","version":"1.2.3"}`,
				},
			},
		},
		{
			Name: "one matching subscription without catalog entry infers no properties",
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
				Spec: v1alpha1.ClusterServiceVersionSpec{
					Version: opver.OperatorVersion{Version: semver.MustParse("1.2.3")},
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package: "x",
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
			},
		},
		{
			Name: "one matching subscription infers package property without csv version",
			Cache: cache.StaticSourceProvider{
				catalog: &cache.Snapshot{
					Entries: []*cache.Entry{
						{
							Name: "a",
							SourceInfo: &cache.OperatorSourceInfo{
								Package: "x",
							},
						},
					},
				},
			},
			CSV: &v1alpha1.ClusterServiceVersion{
				ObjectMeta: metav1.ObjectMeta{
					Name: "a",
				},
			},
			Subscriptions: []*v1alpha1.Subscription{
				{
					Spec: &v1alpha1.SubscriptionSpec{
						Package:                "x",
						CatalogSource:          catalog.Name,
						CatalogSourceNamespace: catalog.Namespace,
					},
					Status: v1alpha1.SubscriptionStatus{
						InstalledCSV: "a",
					},
				},
			},
			Expected: []*api.Property{
				{
					Type:  "olm.package",
					Value: `{"packageName":"x","version":""}`,
				},
			},
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			require := require.New(t)
			logger, _ := test.NewNullLogger()
			s := &csvSource{
				logger: logger,
			}
			actual, err := s.inferProperties(tc.CSV, tc.Subscriptions)
			require.NoError(err)
			require.Equal(tc.Expected, actual)
		})
	}
}
