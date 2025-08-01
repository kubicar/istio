package resources_test

import (
	"context"
	_ "embed"
	networkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"

	"github.com/kyma-project/istio/operator/internal/resources"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apisecurityv1 "istio.io/api/security/v1"
	securityclientv1 "istio.io/client-go/pkg/apis/security/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

//go:embed test_files/resource_with_spec.yaml
var resourceWithSpec []byte

//go:embed test_files/resource_with_data.yaml
var resourceWithData []byte

//go:embed test_files/resource_before_labels.yaml
var resourceBeforeLabels []byte

//go:embed test_files/resource_after_labels.yaml
var resourceAfterLabels []byte

var _ = Describe("Apply", func() {
	It("should create resource with disclaimer", func() {
		// given
		k8sClient := createFakeClient()

		// when
		res, err := resources.Apply(context.Background(), k8sClient, resourceWithSpec, nil)

		// then
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal(controllerutil.OperationResultCreated))

		var pa securityclientv1.PeerAuthentication
		Expect(yaml.Unmarshal(resourceWithSpec, &pa)).Should(Succeed())
		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&pa), &pa)).Should(Succeed())
		um, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pa)
		unstr := unstructured.Unstructured{Object: um}
		Expect(err).ToNot(HaveOccurred())
		ok := resources.HasManagedByDisclaimer(unstr)
		Expect(ok).To(BeTrue())
	})

	It("should create resource containing app.kubernetes.io/version label", func() {
		// given
		k8sClient := createFakeClient()

		// when
		res, err := resources.Apply(context.Background(), k8sClient, resourceWithSpec, nil)

		// then
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal(controllerutil.OperationResultCreated))

		var pa securityclientv1.PeerAuthentication
		Expect(yaml.Unmarshal(resourceWithSpec, &pa)).Should(Succeed())
		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&pa), &pa)).Should(Succeed())
		um, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pa)
		unstr := unstructured.Unstructured{Object: um}
		Expect(err).ToNot(HaveOccurred())

		Expect(unstr.GetLabels()).ToNot(BeNil())
		Expect(unstr.GetLabels()).To(HaveLen(1))
		Expect(unstr.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/version", "dev"))
	})

	It("should update resource with spec and add disclaimer", func() {
		// given
		var pa securityclientv1.PeerAuthentication
		Expect(yaml.Unmarshal(resourceWithSpec, &pa)).Should(Succeed())
		k8sClient := createFakeClient(&pa)

		pa.Spec.Mtls.Mode = apisecurityv1.PeerAuthentication_MutualTLS_PERMISSIVE
		var resourceWithUpdatedSpec []byte
		resourceWithUpdatedSpec, err := yaml.Marshal(&pa)
		Expect(err).ShouldNot(HaveOccurred())

		// when
		res, err := resources.Apply(context.Background(), k8sClient, resourceWithUpdatedSpec, nil)

		// then
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal(controllerutil.OperationResultUpdated))

		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&pa), &pa)).Should(Succeed())
		Expect(pa.Spec.Mtls.Mode).To(Equal(apisecurityv1.PeerAuthentication_MutualTLS_PERMISSIVE))
		um, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pa)
		unstr := unstructured.Unstructured{Object: um}
		Expect(err).ToNot(HaveOccurred())
		ok := resources.HasManagedByDisclaimer(unstr)
		Expect(ok).To(BeTrue())
	})

	It("should append resource with labels if some has been added", func() {
		testLabelKey := "test-label"
		testLabelValue := "test-value"
		var ef networkingv1alpha3.EnvoyFilter
		Expect(yaml.Unmarshal(resourceBeforeLabels, &ef)).Should(Succeed())
		k8sClient := createFakeClient(&ef)

		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&ef), &ef)).Should(Succeed())
		um, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ef)
		unstr := unstructured.Unstructured{Object: um}
		Expect(err).ToNot(HaveOccurred())
		// old label that should be present
		Expect(unstr.GetLabels()).To(HaveKeyWithValue(testLabelKey, testLabelValue))
		// added labels that should not be present yet
		Expect(unstr.GetLabels()).To(Not(HaveKeyWithValue("kyma-project.io/module", "istio")))
		Expect(unstr.GetLabels()).To(Not(HaveKeyWithValue("app.kubernetes.io/component", "operator")))
		Expect(unstr.GetLabels()).To(Not(HaveKeyWithValue("app.kubernetes.io/part-of", "istio")))
		Expect(unstr.GetLabels()).To(Not(HaveKeyWithValue("app.kubernetes.io/name", "istio-operator")))
		Expect(unstr.GetLabels()).To(Not(HaveKeyWithValue("app.kubernetes.io/instance", "istio-operator-default")))

		// when
		res, err := resources.Apply(context.Background(), k8sClient, resourceAfterLabels, nil)

		// then
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal(controllerutil.OperationResultUpdated))

		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&ef), &ef)).Should(Succeed())
		um, err = runtime.DefaultUnstructuredConverter.ToUnstructured(&ef)
		unstr = unstructured.Unstructured{Object: um}
		Expect(err).ToNot(HaveOccurred())
		// old label that should be preserved
		Expect(unstr.GetLabels()).To(HaveKeyWithValue(testLabelKey, testLabelValue))
		//  new labels that should be added
		Expect(unstr.GetLabels()).To(HaveKeyWithValue("kyma-project.io/module", "istio"))
		Expect(unstr.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/component", "operator"))
		Expect(unstr.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/part-of", "istio"))
		Expect(unstr.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/name", "istio-operator"))
		Expect(unstr.GetLabels()).To(HaveKeyWithValue("app.kubernetes.io/instance", "istio-operator-default"))
	})

	It("should update data field of resource and add disclaimer", func() {
		// given
		var cm v1.ConfigMap
		Expect(yaml.Unmarshal(resourceWithData, &cm)).Should(Succeed())
		k8sClient := createFakeClient(&cm)

		cm.Data["some"] = "new-data"
		var resourceWithUpdatedData []byte
		resourceWithUpdatedData, err := yaml.Marshal(&cm)

		Expect(err).ShouldNot(HaveOccurred())

		// when
		res, err := resources.Apply(context.Background(), k8sClient, resourceWithUpdatedData, nil)

		// then
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal(controllerutil.OperationResultUpdated))

		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&cm), &cm)).Should(Succeed())
		Expect(cm.Data["some"]).To(Equal("new-data"))

		um, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cm)
		unstr := unstructured.Unstructured{Object: um}
		Expect(err).ToNot(HaveOccurred())
		ok := resources.HasManagedByDisclaimer(unstr)
		Expect(ok).To(BeTrue())
	})

	It("should set owner reference of resource when owner reference is given", func() {
		// given
		k8sClient := createFakeClient()
		ownerReference := metav1.OwnerReference{
			APIVersion: "security.istio.io/v1",
			Kind:       "PeerAuthentication",
			Name:       "owner-name",
			UID:        "owner-uid",
		}
		// when
		res, err := resources.Apply(context.Background(), k8sClient, resourceWithSpec, &ownerReference)

		// then
		Expect(err).ToNot(HaveOccurred())
		Expect(res).To(Equal(controllerutil.OperationResultCreated))

		var pa securityclientv1.PeerAuthentication
		Expect(yaml.Unmarshal(resourceWithSpec, &pa)).Should(Succeed())
		Expect(k8sClient.Get(context.Background(), ctrlClient.ObjectKeyFromObject(&pa), &pa)).Should(Succeed())
		Expect(pa.OwnerReferences).To(ContainElement(ownerReference))
	})

})
