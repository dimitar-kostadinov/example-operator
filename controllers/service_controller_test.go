package controllers

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"io/ioutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"time"
	// +kubebuilder:scaffold:imports
)

var _ = Describe("Kubernetes operator - controller test", func() {

	const sleepTime = 3 * time.Second

	var (
		err            error
		ctx            context.Context
		mgrCtx         context.Context
		cancel         context.CancelFunc
		name           string
		namespacedName types.NamespacedName
		depl           *appsv1.Deployment
		srv            *corev1.Service
		mgr            manager.Manager
	)

	BeforeEach(func() {
		name = "hello-minikube-test"
		namespacedName = types.NamespacedName{Name: name, Namespace: "default"}
		ctx = context.Background()
		mgrCtx, cancel = context.WithCancel(context.Background())
		By("init manager")
		mgr, err = initManager()
		Expect(err).ToNot(HaveOccurred())
		Expect(mgr).NotTo(BeNil())
		By("init controller")
		err = initControllers(mgr)
		Expect(err).ToNot(HaveOccurred())
		By("start manager")
		go func() {
			err = mgr.Start(mgrCtx)
			Expect(err).ToNot(HaveOccurred())
		}()
		depl = initDeployment(name, 1)
		By(fmt.Sprintf("create %s deployment", name))
		err = k8sClient.Create(ctx, depl)
		Expect(err).ToNot(HaveOccurred())
		srv = initLoadBalancerService(name, 30080, 8080)
		By(fmt.Sprintf("expose %s deployment", name))
		err = k8sClient.Create(ctx, srv)
		Expect(err).ToNot(HaveOccurred())
		time.Sleep(sleepTime)
	})

	AfterEach(func() {
		By(fmt.Sprintf("delete %s service", name))
		err = k8sClient.Delete(ctx, srv)
		Expect(err).ToNot(HaveOccurred())
		By(fmt.Sprintf("delete %s deployment", name))
		err = k8sClient.Delete(ctx, depl)
		Expect(err).ToNot(HaveOccurred())
		time.Sleep(sleepTime)
		By("stop manager")
		cancel()
	})

	It("should access exposed deployment", func() {
		s := corev1.Service{}
		getService(ctx, namespacedName, &s)
		accessExposedDeployment(s.Status.LoadBalancer.Ingress[0].IP, getSrv(namespacedName).port)
	})

	Context("When the service node port changes", func() {

		var (
			nodePort int32
			port     int
			lbIP     string
		)

		BeforeEach(func() {
			s := corev1.Service{}
			getService(ctx, namespacedName, &s)
			lbIP = s.Status.LoadBalancer.Ingress[0].IP
			port = getSrv(namespacedName).port
			accessExposedDeployment(lbIP, port)
			Expect(len(s.Spec.Ports) > 0 && s.Spec.Ports[0].NodePort > 0).To(BeTrue())
			nodePort = s.Spec.Ports[0].NodePort - 1 // assume the preceding port will be free
			s.Spec.Ports[0].NodePort = nodePort
			err = k8sClient.Update(ctx, &s)
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(sleepTime)
		})

		It("should access exposed deployment", func() {
			accessExposedDeployment(lbIP, port)
		})
	})
})

func initDeployment(name string, replicas int) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	rep := new(int32)
	*rep = int32(replicas)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: rep,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "echoserver", Image: "k8s.gcr.io/echoserver:1.4"}},
				},
			},
		},
	}
}

func initLoadBalancerService(name string, port, targetPort int) *corev1.Service {
	labels := map[string]string{"app": name}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: name, Protocol: corev1.ProtocolTCP, Port: int32(port), TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: int32(targetPort)}},
			},
			Selector: labels,
			Type:     corev1.ServiceTypeLoadBalancer,
		},
	}
}

func initManager() (mgr manager.Manager, err error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	mgr, err = ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     ":8080",
		Port:                   9443,
		HealthProbeBindAddress: ":8081",
		LeaderElection:         false,
	})
	return
}

func initControllers(mgr manager.Manager) (err error) {
	if err = (&ServiceReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Service"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return
	}
	err = (&NodeReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Node"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	return
}

func getService(ctx context.Context, namespacedName types.NamespacedName, s *corev1.Service) {
	err := k8sClient.Get(ctx, namespacedName, s)
	Expect(err).ToNot(HaveOccurred())
	Expect(s).NotTo(BeNil())
	Expect(len(s.Status.LoadBalancer.Ingress)).To(Equal(1))
	Expect(s.Status.LoadBalancer.Ingress[0].IP).NotTo(BeEmpty())
	Expect(s.Finalizers).To(ContainElement(serviceFinalizer))
}

func getSrv(namespacedName types.NamespacedName) *Server {
	srv, ok := getServer(namespacedName.String())
	Expect(ok).To(BeTrue())
	Expect(srv).NotTo(BeNil())
	Expect(srv.port > 0).To(BeTrue())
	return srv
}

func accessExposedDeployment(ip string, port int) {
	resp, err := http.Get(fmt.Sprintf("http://%s:%d", ip, port))
	Expect(err).ToNot(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	content, err := ioutil.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	Expect(string(content)).To(ContainSubstring("host=%s:%d", ip, port))
	err = resp.Body.Close()
	Expect(err).NotTo(HaveOccurred())
}
