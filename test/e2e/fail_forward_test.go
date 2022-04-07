package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/blang/semver/v4"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/bundle"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry/resolver/cache"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorclient"
	"github.com/operator-framework/operator-lifecycle-manager/test/e2e/ctx"
	registry2 "github.com/operator-framework/operator-registry/pkg/registry"
)

var _ = Describe("Fail Forward with toggle", func() {
	var (
		kubeclient operatorclient.ClientInterface
		crclient   versioned.Interface
		client     client.Client
		testOG     string
		cleanupNS  cleanupFunc
	)
	type ffStep struct {
		entry                    *cache.Entry
		failForwardStrategy      *operatorsv1.UpgradeStrategy
		disableFailForward       bool
		installPlanTimeout       *time.Duration
		expectedInstallPlanPhase *operatorsv1alpha1.InstallPlanPhase
		expectedCSVPhase         *operatorsv1alpha1.ClusterServiceVersionPhase
	}

	crdPlural := genName("ins-")
	crdName := crdPlural + ".cluster.com"
	crd := apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "cluster.com",
			Scope: apiextensionsv1.ClusterScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   crdPlural,
				Singular: crdPlural,
				Kind:     crdPlural,
				ListKind: "list" + crdPlural,
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
					},
				},
			}},
		},
	}

	crds := []apiextensionsv1.CustomResourceDefinition{crd}

	runSteps := func(crclient versioned.Interface, namespace, testPkg string, steps []ffStep) {
		catSrcName := genName(fmt.Sprintf("%s-catsrc-", testPkg))
		subscriptionName := genName(fmt.Sprintf("%s-sub-", testPkg))

		subscriptionSpec := &operatorsv1alpha1.SubscriptionSpec{
			CatalogSource:          catSrcName,
			CatalogSourceNamespace: namespace,
			Package:                testPkg,
			Channel:                stableChannel,
			// to allow faster e2e tests, bundle unpack time must be set as an annotation on
			// a generated installPlan before it is processed. Set approval to manual to ensure enough time.
			InstallPlanApproval: operatorsv1alpha1.ApprovalManual,
		}

		createSubscriptionForCatalogWithSpec(GinkgoT(), crclient, namespace, subscriptionName, subscriptionSpec)

		var currentInstallPlanName string
		for _, step := range steps {
			// 1. Change OperatorGroup toggle if different from step value
			// 2. if a new bundle is specified, add it to the catalog
			// 3. If a new installPlan is meant to be generated
			// 	  3.1 wait till subscription updates with latest installPlan
			//    3.2 Set bundleUnpack timeout on installPlan
			//    3.3 Approve installPlan
			// 4. ensure installPlan transitions to required phase
			// 5. ensure csv is in the correct phase

			if step.failForwardStrategy != nil {
				setFailForwardOG(context.Background(), crclient, testNamespace, testOG, *step.failForwardStrategy)
			}

			if step.entry != nil {
				_, err := addEntryToCatalog(context.Background(), client, kubeclient, crclient, *step.entry, namespace, catSrcName, crds)
				Expect(err).Should(BeNil())
			}

			if step.expectedInstallPlanPhase != nil {
				// Wait for InstallPlan generation
				subscription, err := fetchSubscription(crclient, namespace, subscriptionName, func(subscription *operatorsv1alpha1.Subscription) bool {
					return subscriptionHasInstallPlanChecker(subscription)
				})
				Expect(err).Should(BeNil())

				currentInstallPlanName = subscription.Status.InstallPlanRef.Name

				if step.installPlanTimeout != nil {
					setBundleUnpackTimeoutForInstallPlan(context.Background(), crclient, namespace, currentInstallPlanName, step.installPlanTimeout.String())
				}
				approveInstallPlan(context.Background(), crclient, namespace, currentInstallPlanName)

				_, err = fetchInstallPlan(GinkgoT(), crclient, currentInstallPlanName, namespace, buildInstallPlanPhaseCheckFunc(*step.expectedInstallPlanPhase))
				Expect(err).ToNot(HaveOccurred(), "failed to wait until the installplan reached the expected phase")

				if step.expectedCSVPhase != nil {
					_, err = awaitCSV(crclient, namespace, step.entry.Name, buildCSVConditionChecker(*step.expectedCSVPhase))
					Expect(err).Should(BeNil())
				} else {
					_, err = crclient.OperatorsV1alpha1().ClusterServiceVersions(namespace).Get(context.Background(), step.entry.Name, metav1.GetOptions{})
					Expect(errors.IsNotFound(err)).Should(BeTrue())
				}
			} else {
				// No new installPlan should be generated.
				subscription, err := fetchSubscription(crclient, namespace, subscriptionName, subscriptionHasInstallPlanChecker)
				Expect(err).Should(BeNil())
				if subscription.Status.InstallPlanRef == nil {
					Expect(currentInstallPlanName).Should(BeEmpty())
				} else {
					Expect(subscription.Status.InstallPlanRef.Name).Should(Equal(currentInstallPlanName))
				}
			}
		}
	}

	// withInvalidCSV := cache.Entry{
	// 	BundlePath: "quay.io/does/not:exist",
	// }
	withInvalidIP := cache.Entry{
		ProvidedAPIs: map[registry2.APIKey]struct{}{
			{
				Group:   "n",
				Version: "o",
				Kind:    "p",
				Plural:  "e",
			}: {},
		},
	}

	populateEntry := func(base cache.Entry, pkg, name, version, replaces, skipRange string, skips []string) *cache.Entry {
		base.Name = name
		base.Replaces = replaces
		base.Skips = skips
		if len(version) > 0 {
			parsedVersion, err := semver.Parse(version)
			Expect(err).Should(BeNil())
			base.Version = &parsedVersion
		}
		if len(skipRange) > 0 {
			parsedRange, err := semver.ParseRange(skipRange)
			Expect(err).Should(BeNil())
			base.SkipRange = parsedRange
		}
		base.SourceInfo = &cache.OperatorSourceInfo{
			Package: pkg,
			Channel: stableChannel,
		}
		return &base
	}

	BeforeEach(func() {
		kubeclient = newKubeClient()
		crclient = newCRClient()
		client = ctx.Ctx().Client()

		testNamespace = genName("ff-ns-")
		_, cleanupNS = newNamespace(kubeclient, testNamespace)

		testOG = genName("ff-og-")
		newOperatorGroupWithServiceAccount(crclient, testNamespace, testOG, "")

		setFailForwardOG(context.Background(), crclient, testNamespace, testOG, operatorsv1.UpgradeStrategy{Name: operatorsv1.UnsafeFailForwardUpgradeStrategy})
	})

	AfterEach(func() {
		TearDown(testNamespace)
		cleanupNS()
	})

	//og without toggle
	It("Must perform fail forward upgrade for failed installPlan", func() {
		testPkg := genName("ff-operator-")
		shortFailForward := 1 * time.Second
		ipComplete := operatorsv1alpha1.InstallPlanPhaseComplete
		ipFailed := operatorsv1alpha1.InstallPlanPhaseFailed
		csvSucceeded := operatorsv1alpha1.CSVPhaseSucceeded
		runSteps(crclient, testNamespace, testPkg, []ffStep{
			{
				entry:                    populateEntry(cache.Entry{}, testPkg, "test.v1.0.0", "1.0.0", "", "", nil),
				expectedInstallPlanPhase: &ipComplete,
				expectedCSVPhase:         &csvSucceeded,
			},
			{
				entry:                    populateEntry(withInvalidIP, testPkg, "test.v1.0.1", "1.0.1", "test.v1.0.0", "", nil),
				installPlanTimeout:       &shortFailForward,
				expectedInstallPlanPhase: &ipFailed,
			},
			{
				entry:                    populateEntry(withInvalidIP, testPkg, "test.v1.0.2", "1.0.2", "test.v1.0.1", "<1.0.2", nil),
				installPlanTimeout:       &shortFailForward,
				expectedInstallPlanPhase: &ipFailed,
			},
			{
				entry:                    populateEntry(cache.Entry{}, testPkg, "test.v1.0.3", "1.0.3", "test.v1.0.2", "<1.0.3", nil),
				expectedInstallPlanPhase: &ipComplete,
				expectedCSVPhase:         &csvSucceeded,
			},
		})

	})

	It("Must fail forward upgrade for failed CSV", func() {
		testPkg := genName("ff-operator-")
		ipComplete := operatorsv1alpha1.InstallPlanPhaseComplete
		// ipFailed := operatorsv1alpha1.InstallPlanPhaseFailed
		csvSucceeded := operatorsv1alpha1.CSVPhaseSucceeded
		// csvFailed := operatorsv1alpha1.CSVPhaseFailed
		runSteps(crclient, testNamespace, testPkg, []ffStep{
			{
				entry:                    populateEntry(cache.Entry{}, testPkg, "test.v1.0.0", "1.0.0", "", "", nil),
				expectedInstallPlanPhase: &ipComplete,
				expectedCSVPhase:         &csvSucceeded,
			},
			// {
			// 	entry:                    populateEntry(withInvalidCSV, testPkg, "test.v1.0.1", "1.0.1", "test.v1.0.0", "", nil),
			// 	expectedInstallPlanPhase: &ipFailed,
			// 	expectedCSVPhase:         &csvFailed,
			// },
			// {
			// 	entry:                    populateEntry(withInvalidCSV, testPkg, "test.v1.0.2", "1.0.2", "test.v1.0.1", "<1.0.2", nil),
			// 	expectedInstallPlanPhase: &ipFailed,
			// 	expectedCSVPhase:         &csvFailed,
			// },
			// {
			// 	entry:                    populateEntry(cache.Entry{}, testPkg, "test.v1.0.3", "1.0.3", "test.v1.0.2", "<1.0.3", nil),
			// 	expectedInstallPlanPhase: &ipComplete,
			// 	expectedCSVPhase:         &csvSucceeded,
			// },
		})
	})
})

func unpackCatSrcConfigMap(configMap *corev1.ConfigMap) ([]registry.PackageManifest, []apiextensionsv1.CustomResourceDefinition, []operatorsv1alpha1.ClusterServiceVersion) {
	var manifests []registry.PackageManifest
	var crds []apiextensionsv1.CustomResourceDefinition
	var csvs []operatorsv1alpha1.ClusterServiceVersion
	if configMap == nil || len(configMap.Data) == 0 {
		return manifests, crds, csvs
	}
	if len(configMap.Data[registry.ConfigMapPackageName]) > 0 {
		err := yaml.Unmarshal([]byte(configMap.Data[registry.ConfigMapPackageName]), &manifests)
		Expect(err).Should(BeNil())
	}
	if len(configMap.Data[registry.ConfigMapCRDName]) > 0 {
		err := yaml.Unmarshal([]byte(configMap.Data[registry.ConfigMapCRDName]), &crds)
		Expect(err).Should(BeNil())
	}
	if len(configMap.Data[registry.ConfigMapCSVName]) > 0 {
		err := yaml.Unmarshal([]byte(configMap.Data[registry.ConfigMapCSVName]), &csvs)
		Expect(err).Should(BeNil())
	}
	return manifests, crds, csvs
}

var (
	c = ctx.Ctx()
)

func addEntryToCatalog(ctx context.Context, c client.Client, kubeclient operatorclient.ClientInterface, crclient versioned.Interface, entry cache.Entry, namespace, catsrc string, crds []apiextensionsv1.CustomResourceDefinition) (cleanupFunc, error) {
	csv := csvForEntry(namespace, entry, crds)

	catalogSource, err := crclient.OperatorsV1alpha1().CatalogSources(namespace).Get(ctx, catsrc, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if apierrors.IsNotFound(err) {
		// c.Logf("creating the %s/%s internal catalogsource", catsrc, namespace)
		_, cleanup := createV1CRDInternalCatalogSource(GinkgoT(), kubeclient, crclient, catsrc, namespace, []registry.PackageManifest{{
			PackageName:        entry.Package(),
			Channels:           []registry.PackageChannel{{Name: entry.Channel(), CurrentCSVName: entry.Name}},
			DefaultChannelName: stableChannel,
		}},
			crds,
			[]operatorsv1alpha1.ClusterServiceVersion{*csv},
		)
		return cleanup, nil
	}

	// c.Logf("updating the existing %s/%s with a new catalog entry", catsrc, namespace)
	// merge csvs, crds and manifests with existing ones
	configMap, err := kubeclient.GetConfigMap(namespace, catalogSource.Spec.ConfigMap)
	Expect(err).Should(BeNil())
	manifests, oldCRDs, oldCSVs := unpackCatSrcConfigMap(configMap)

	// ensure uniqueness for crds and csvs
	addedCRDs := map[string]struct{}{}
	var allCRDs []apiextensionsv1.CustomResourceDefinition
	for _, c := range append(oldCRDs, crds...) {
		if _, ok := addedCRDs[fmt.Sprintf("%s-%s-", c.Namespace, c.Name)]; !ok {
			addedCRDs[fmt.Sprintf("%s-%s-", c.Namespace, c.Name)] = struct{}{}
			allCRDs = append(allCRDs, c)
		}
	}

	addedCSVs := map[string]struct{}{}
	var allCSVs []operatorsv1alpha1.ClusterServiceVersion
	for _, c := range append(oldCSVs, *csv) {
		if _, ok := addedCSVs[fmt.Sprintf("%s-%s-", c.Namespace, c.Name)]; !ok {
			addedCSVs[fmt.Sprintf("%s-%s-", c.Namespace, c.Name)] = struct{}{}
			allCSVs = append(allCSVs, c)
		}
	}

	var foundManifest bool
	for m := range manifests {
		if manifests[m].PackageName == entry.Package() {
			for c := range manifests[m].Channels {
				if manifests[m].Channels[c].Name == entry.Channel() {
					manifests[m].Channels[c].CurrentCSVName = entry.Name
					foundManifest = true
					break
				}
			}
			if !foundManifest {
				manifests[m].Channels = append(manifests[m].Channels, registry.PackageChannel{Name: entry.Channel(), CurrentCSVName: entry.Name})
				foundManifest = true
				break
			}
		}
	}
	if !foundManifest {
		manifests = append(manifests, registry.PackageManifest{
			PackageName:        entry.Package(),
			Channels:           []registry.PackageChannel{{Name: entry.Channel(), CurrentCSVName: entry.Name}},
			DefaultChannelName: stableChannel,
		})
	}

	// TODO: add code for recycling the registry pod
	// TODO: manually approve the newly generated InstallPlan resource

	// confMap, _ := createV1CRDConfigMapForCatalogData(GinkgoT(), kubeclient, genName(catsrc), namespace, manifests, allCRDs, allCSVs)
	// catalogSource.Spec.ConfigMap = confMap.Name
	// _, err = crclient.OperatorsV1alpha1().CatalogSources(namespace).Update(ctx, catalogSource, metav1.UpdateOptions{})
	// Expect(err).Should(BeNil())

	// confMap, _ := createV1CRDConfigMapForCatalogData(GinkgoT(), kubeclient, catalogSource.Name, catalogSource.Namespace, manifests, allCRDs, allCSVs)
	// Eventually(func() error {
	// 	cs, err := crclient.OperatorsV1alpha1().CatalogSources(namespace).Get(ctx, catalogSource.Name, metav1.GetOptions{})
	// 	if err != nil {
	// 		return err
	// 	}
	// 	cs.Spec.ConfigMap = confMap.Name
	// 	_, err = crclient.OperatorsV1alpha1().CatalogSources(namespace).Update(ctx, cs, metav1.UpdateOptions{})
	// 	return err
	// })

	_, cleanup := updateV1CRDConfigMapForCatalogData(GinkgoT(), kubeclient, catalogSource.Spec.ConfigMap, namespace, manifests, allCRDs, allCSVs)

	// podList := &corev1.PodList{}
	// if err := c.List(ctx, podList, &client.ListOptions{
	// 	LabelSelector: labels.NewSelector(),
	// })

	// olm.catalogSource=ff-operator-hhb7g-catsrc-zrzg2,olm.configMapResourceVersion=127362,olm.pod-spec-hash=6bb8976b7b
	// TODO: delete the registry pod

	return cleanup, err
}

func setFailForwardOG(ctx context.Context, crclient versioned.Interface, namespace, name string, upgradeStrategy operatorsv1.UpgradeStrategy) {
	_, err := crclient.OperatorsV1().OperatorGroups(namespace).Patch(ctx, name, types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"upgradeStrategy":{"name":"%s"}}}`, upgradeStrategy.Name)), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func approveInstallPlan(ctx context.Context, crclient versioned.Interface, namespace string, ipName string) {
	ip, err := fetchInstallPlanWithNamespace(GinkgoT(), crclient, ipName, namespace, buildInstallPlanPhaseCheckFunc(operatorsv1alpha1.InstallPlanPhaseRequiresApproval))
	Expect(err).Should(BeNil())
	ip.Spec.Approved = true
	_, err = crclient.OperatorsV1alpha1().InstallPlans(namespace).Update(ctx, ip, metav1.UpdateOptions{})
	Expect(err).Should(BeNil())
}

func setBundleUnpackTimeoutForInstallPlan(ctx context.Context, crclient versioned.Interface, namespace string, ipName string, timeout string) {
	ip, err := fetchInstallPlanWithNamespace(GinkgoT(), crclient, ipName, namespace, buildInstallPlanPhaseCheckFunc(operatorsv1alpha1.InstallPlanPhaseRequiresApproval))
	Expect(err).Should(BeNil())

	if ip.Annotations == nil {
		ip.Annotations = make(map[string]string, 1)
	}
	ip.Annotations[bundle.BundleUnpackTimeoutAnnotationKey] = timeout

	_, err = crclient.OperatorsV1alpha1().InstallPlans(namespace).Update(ctx, ip, metav1.UpdateOptions{})
	Expect(err).Should(BeNil())
}

func csvForEntry(namespace string, e cache.Entry, crds []apiextensionsv1.CustomResourceDefinition) *operatorsv1alpha1.ClusterServiceVersion {
	permissions := deploymentPermissions()
	namedInstallStrategy := newNginxInstallStrategy(genName("ff-"), permissions, nil)
	csv := newCSV(e.Name, namespace, e.Replaces, *e.Version, nil, nil, &namedInstallStrategy)
	csv.Spec.Skips = e.Skips
	if e.SkipRange != nil {
		csv.Annotations[operatorsv1alpha1.SkipRangeAnnotationKey] = fmt.Sprintf("%v", e.SkipRange)
	}
	if len(e.BundlePath) > 0 {
		csv.Spec.RelatedImages = append(csv.Spec.RelatedImages, operatorsv1alpha1.RelatedImage{Name: "bundlePath", Image: e.BundlePath})
	}
	for apiKey := range e.ProvidedAPIs {
		csv.Spec.APIServiceDefinitions.Owned = append(csv.Spec.APIServiceDefinitions.Owned, operatorsv1alpha1.APIServiceDescription{
			Name:    apiKey.Plural,
			Group:   apiKey.Group,
			Version: apiKey.Version,
			Kind:    apiKey.Kind,
		})
	}
	for apiKey := range e.RequiredAPIs {
		csv.Spec.APIServiceDefinitions.Required = append(csv.Spec.APIServiceDefinitions.Required, operatorsv1alpha1.APIServiceDescription{
			Name:    apiKey.Plural,
			Group:   apiKey.Group,
			Version: apiKey.Version,
			Kind:    apiKey.Kind,
		})
	}
	for _, crd := range crds {
		crdVersion := "v1alpha1"
		for _, v := range crd.Spec.Versions {
			if v.Served && v.Storage {
				crdVersion = v.Name
				break
			}
		}
		desc := operatorsv1alpha1.CRDDescription{
			Name:        crd.GetName(),
			Version:     crdVersion,
			Kind:        crd.Spec.Names.Plural,
			DisplayName: crd.GetName(),
			Description: crd.GetName(),
		}
		csv.Spec.CustomResourceDefinitions.Owned = append(csv.Spec.CustomResourceDefinitions.Owned, desc)
	}

	return &csv
}
