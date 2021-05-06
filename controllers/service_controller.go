/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-logr/logr"
	"io"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sync"
)

const (
	minikubeNodeName = "minikube"
	serviceFinalizer = "service.example-operator.com/finalizer"
)

var (
	machineIP string
	// map contains service NamespacedName -> Server
	servers = make(map[string]*Server)
	// thread safe lock for servers map access (controller.MaxConcurrentReconciles may be set > 1)
	mux = sync.RWMutex{}
)

// servers map operations
func getServer(key string) (srv *Server, ok bool) {
	mux.RLock()
	defer mux.RUnlock()
	srv, ok = servers[key]
	return
}

func addServer(key string, srv *Server) {
	mux.Lock()
	mux.Unlock()
	servers[key] = srv
}

func deleteServer(key string) {
	mux.Lock()
	mux.Unlock()
	delete(servers, key)
}

// Server type & functions
type Server struct {
	log        logr.Logger
	listener   net.Listener
	dialer     net.Dialer
	minikubeIP string
	port       int
	nodePort   int32
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// start listening on machineIP : port
func (s *Server) serve() {
	defer s.wg.Done()
	s.log.Info(fmt.Sprintf("server listen on port %d", s.port))
	for {
		c, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				s.log.Info(fmt.Sprintf("context done: %v", s.ctx.Err()))
				return
			default:
				s.log.Error(err, "could not accept client connection")
				continue
			}
		}
		s.wg.Add(1)
		go s.handler(c)
	}
}

// dail minikubeIP : nodePort and start forward the traffic
func (s *Server) handler(c net.Conn) {
	log := s.log.WithValues("connection", c)

	defer s.wg.Done()
	defer func() {
		err := c.Close()
		if err != nil {
			log.Error(err, "close connection")
		}
	}()

	target, err := s.dialer.DialContext(s.ctx, "tcp", fmt.Sprintf("%s:%d", s.minikubeIP, s.nodePort))
	if err != nil {
		log.Error(err, "could not connect to target")
		return
	}
	defer func() {
		err := target.Close()
		if err != nil {
			log.Error(err, "close target connection")
		}
	}()
	// forward target > connection
	go func() {
		_, err = io.Copy(target, c)
		if err != nil {
			log.Error(err, "copy from target to connection")
		}
	}()
	// forward connection > target
	_, err = io.Copy(c, target)
	if err != nil {
		log.Error(err, "copy from connection to target")
	}
}

// stop the server
// cancel server ctx & invoke cleanup with server ctx
func (s *Server) stop() error {
	s.log.Info("stopping server ...")
	defer s.log.Info("server stopped")
	s.cancel()
	return s.cleanup(s.ctx)
}

// wait for ctx done, close the listener
// and wait for active connections to complete
// returns err if listener close fails
func (s *Server) cleanup(ctx context.Context) error {
	<-ctx.Done()
	err := s.listener.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		s.log.Error(err, "could not close listener")
		return err
	}
	s.wg.Wait()
	s.log.Info("end close")
	return nil
}

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("service", req.NamespacedName)

	key := req.NamespacedName.String()
	service := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		if apierrs.IsNotFound(err) {
			log.Info("resource not found")
			// we'll ignore not-found errors, since they can't be fixed by an immediate
			// requeue (we'll need to wait for a new notification), and we can get them
			// on deleted requests.
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Service")
		return ctrl.Result{}, err
	}

	if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
		// first check if DeletionTimestamp is set
		if service.ObjectMeta.DeletionTimestamp != nil {
			log.Info("Service resource is marked for deletion")
			if controllerutil.ContainsFinalizer(service, serviceFinalizer) {
				// our finalizer is present, so lets handle any external dependency
				if err := r.finalizeService(key); err != nil {
					// if fail to delete the external dependency here, return with error
					// so that it can be retried
					return ctrl.Result{}, err
				}
				// remove our finalizer from the list and update it.
				controllerutil.RemoveFinalizer(service, serviceFinalizer)
				if err := r.Update(ctx, service); err != nil {
					return ctrl.Result{}, err
				}
			}
			// Stop reconciliation as the item is being deleted
			return ctrl.Result{}, nil
		} else {
			// The object is not being deleted, so if it does not have our finalizer,
			// then lets add the finalizer and update the object. This is equivalent
			// registering our finalizer.
			if !controllerutil.ContainsFinalizer(service, serviceFinalizer) {
				controllerutil.AddFinalizer(service, serviceFinalizer)
				if err := r.Update(ctx, service); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		// init machine IP
		if err := initMachineIP(log); err != nil {
			log.Error(err, "initialize machine IP")
			return ctrl.Result{}, err
		}
		// get minikube node IP
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Namespace: "", Name: minikubeNodeName}, &node); err != nil {
			log.Error(err, "failed to get minikube Node")
			return ctrl.Result{}, err
		}
		if len(node.Status.Addresses) == 0 || len(node.Status.Addresses[0].Address) == 0 {
			err := errors.New("minikube Node address not yet set")
			log.Error(err, "get minikube IP")
			return ctrl.Result{}, err
		}
		minikubeIP := node.Status.Addresses[0].Address
		log.Info(fmt.Sprintf("minikube Node IP: %s", minikubeIP))
		// get node port
		if len(service.Spec.Ports) == 0 || service.Spec.Ports[0].NodePort == 0 {
			err := errors.New("node port not yet set")
			log.Error(err, "get service node port")
			return ctrl.Result{}, err
		}
		nodePort := service.Spec.Ports[0].NodePort
		log.Info(fmt.Sprintf("service node port: %d", nodePort))

		// check if server is already created
		if server, ok := getServer(key); ok {
			log.Info("server already created")
			updateServer(log, server, minikubeIP, nodePort)
		} else {
			log.Info("create and start server")
			if err := createServer(ctx, log, minikubeIP, nodePort, key); err != nil {
				return ctrl.Result{}, err
			}
		}

		// update LoadBalancer service status
		lbStatus := corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{
				IP: machineIP,
			}},
		}
		if !reflect.DeepEqual(lbStatus, service.Status.LoadBalancer) {
			log.Info("updating LoadBalancer status")
			// The obj returned in r.Get is supposed to be a deep copy of the cache object, however this comment is suspicious
			// https://github.com/kubernetes-sigs/controller-runtime/blob/b2c90ab82af89fb84120108af14f4ade9df5d787/pkg/cache/internal/cache_reader.go#L84
			service.Status.LoadBalancer = lbStatus
			err := r.Status().Update(ctx, service)
			if err != nil {
				if apierrs.IsConflict(err) {
					log.Info("retry applying changes to the latest Service version")
				} else {
					log.Error(err, "failed to update Service LoadBalancer status")
				}
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}

// initialize machine IP
func initMachineIP(log logr.Logger) error {
	if len(machineIP) == 0 {
		ifaces, err := net.Interfaces()
		if err != nil {
			return err
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue // interface down
			}
			if iface.Flags&net.FlagLoopback != 0 {
				continue // loopback interface
			}
			addrs, err := iface.Addrs()
			if err != nil {
				return err
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}
				ip = ip.To4()
				if ip == nil {
					continue // not an ipv4 address
				}
				machineIP = ip.String()
				log.Info(fmt.Sprintf("machine IP:%s", machineIP))
				return nil
			}
		}
		return errors.New("no IP found")
	}
	return nil
}

// delete any external resources associated with the Service
// Ensure that delete implementation is idempotent and safe to invoke
// multiple times for same object.
func (r *ServiceReconciler) finalizeService(key string) (err error) {
	if server, ok := getServer(key); ok {
		err = server.stop()
		if err != nil {
			return
		}
		deleteServer(key)
	}
	return
}

// updating Server with minikubeIP & nodePort
func updateServer(log logr.Logger, server *Server, minikubeIP string, nodePort int32) {
	// update minikubeIP
	if server.minikubeIP != minikubeIP {
		log.Info(fmt.Sprintf("updating minikube IP from %s to %s", server.minikubeIP, minikubeIP))
		server.minikubeIP = minikubeIP
	}
	// update nodePort
	if nodePort > 0 {
		if server.nodePort != nodePort {
			log.Info(fmt.Sprintf("updating node port from %d to %d", server.nodePort, nodePort))
			server.nodePort = nodePort
		}
	}
}

// create & start Server
func createServer(ctx context.Context, log logr.Logger, minikubeIP string, nodePort int32, key string) error {
	server := &Server{
		log:        log.WithName("server"),
		dialer:     net.Dialer{KeepAlive: 0},
		minikubeIP: minikubeIP,
		nodePort:   nodePort,
	}
	// propagate the context to the server
	server.ctx, server.cancel = context.WithCancel(ctx)
	// listener
	listenCfg := &net.ListenConfig{KeepAlive: 0}
	var err error
	server.listener, err = listenCfg.Listen(server.ctx, "tcp", fmt.Sprintf("%s:0", machineIP))
	if err != nil {
		log.Error(err, fmt.Sprintf("listen on %s:0", machineIP))
		return err
	}
	tcpAddr, ok := server.listener.Addr().(*net.TCPAddr)
	if !ok {
		err = fmt.Errorf("not a TCPAddr: %T", server.listener.Addr())
		log.Error(err, "unexpected listener")
		return err
	}
	server.port = tcpAddr.Port
	log.Info(fmt.Sprintf("dynamically allocated port: %d", server.port))
	addServer(key, server)
	server.wg.Add(1)
	go server.serve()
	go func() {
		// wait for context done (e.g. Ctrl-C SIGINT) & cleanup Server resources
		_ = server.cleanup(ctx)
	}()
	return nil
}
