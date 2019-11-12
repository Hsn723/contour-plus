package controllers

import (
	"context"
	"net"

	"github.com/go-logr/logr"
	certmanagerv1alpha2 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	"github.com/kubernetes-incubator/external-dns/endpoint"
	projectcontourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// HTTPProxyReconciler reconciles a HttpProxy object
type HTTPProxyReconciler struct {
	client.Client
	Log               logr.Logger
	Scheme            *runtime.Scheme
	ServiceKey        client.ObjectKey
	IssuerKey         client.ObjectKey
	Prefix            string
	DefaultIssuerName string
	DefaultIssuerKind string
	CreateDNSEndpoint bool
	CreateCertificate bool
}

// +kubebuilder:rbac:groups=projectcontour.io,resources=httpproxies,verbs=get;list;watch
// +kubebuilder:rbac:groups=projectcontour.io,resources=httpproxies/status,verbs=get
// +kubebuilder:rbac:groups=externaldns.k8s.io,resources=dnsendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get

// Reconcile creates/updates CRDs from given HTTPProxy
func (r *HTTPProxyReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("httpproxy", req.NamespacedName)

	// Get HTTPProxy
	hp := new(projectcontourv1.HTTPProxy)
	objKey := client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}
	err := r.Get(ctx, objKey, hp)
	if k8serrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		log.Error(err, "unable to get HTTPProxy resources")
		return ctrl.Result{}, err
	}

	if hp.Annotations[excludeAnnotation] == "true" {
		return ctrl.Result{}, nil
	}

	if err := r.reconcileDNSEndpoint(ctx, hp, log); err != nil {
		log.Error(err, "unable to reconcile DNSEndpoint")
		return ctrl.Result{}, err
	}

	if err := r.reconcileCertificate(ctx, hp, log); err != nil {
		log.Error(err, "unable to reconcile Certificate")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *HTTPProxyReconciler) reconcileDNSEndpoint(ctx context.Context, hp *projectcontourv1.HTTPProxy, log logr.Logger) error {
	if !r.CreateDNSEndpoint {
		return nil
	}

	if hp.Spec.VirtualHost == nil {
		return nil
	}
	fqdn := hp.Spec.VirtualHost.Fqdn
	if len(fqdn) == 0 {
		return nil
	}

	// Get IP list of loadbalancer Service
	var serviceIPs []net.IP
	var svc corev1.Service
	err := r.Get(ctx, r.ServiceKey, &svc)
	if err != nil {
		return err
	}

	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if len(ing.IP) == 0 {
			continue
		}
		serviceIPs = append(serviceIPs, net.ParseIP(ing.IP))
	}
	if len(serviceIPs) == 0 {
		log.Info("no IP address for service " + r.ServiceKey.String())
		// we can return nil here because the controller will be notified
		// as soon as a new IP address is assigned to the service.
		return nil
	}

	de := &endpoint.DNSEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hp.Namespace,
			Name:      r.Prefix + hp.Name,
		},
	}
	op, err := ctrl.CreateOrUpdate(ctx, r.Client, de, func() error {
		de.Spec.Endpoints = makeEndpoints(fqdn, serviceIPs)
		return ctrl.SetControllerReference(hp, de, r.Scheme)
	})
	if err != nil {
		return err
	}

	log.Info("DNSEndpoint successfully reconciled", "operation", op)
	return nil
}

func (r *HTTPProxyReconciler) reconcileCertificate(ctx context.Context, hp *projectcontourv1.HTTPProxy, log logr.Logger) error {
	if !r.CreateCertificate {
		return nil
	}
	if hp.Annotations[testACMETLSAnnotation] != "true" {
		return nil
	}

	vh := hp.Spec.VirtualHost
	switch {
	case vh == nil:
		return nil
	case vh.Fqdn == "":
		return nil
	case vh.TLS == nil:
		return nil
	case vh.TLS.SecretName == "":
		return nil
	}

	issuerName := r.DefaultIssuerName
	issuerKind := r.DefaultIssuerKind
	if name, ok := hp.Annotations[issuerNameAnnotation]; ok {
		issuerName = name
		issuerKind = certmanagerv1alpha2.IssuerKind
	}
	if name, ok := hp.Annotations[clusterIssuerNameAnnotation]; ok {
		issuerName = name
		issuerKind = certmanagerv1alpha2.ClusterIssuerKind
	}

	if issuerName == "" {
		log.Info("no issuer name")
		return nil
	}

	crt := &certmanagerv1alpha2.Certificate{}
	crt.SetNamespace(hp.Namespace)
	crt.SetName(r.Prefix + hp.Name)
	op, err := ctrl.CreateOrUpdate(ctx, r.Client, crt, func() error {
		crt.Spec.DNSNames = []string{vh.Fqdn}
		crt.Spec.SecretName = vh.TLS.SecretName
		crt.Spec.CommonName = vh.Fqdn
		crt.Spec.IssuerRef.Name = issuerName
		crt.Spec.IssuerRef.Kind = issuerKind
		return ctrl.SetControllerReference(hp, crt, r.Scheme)
	})
	if err != nil {
		return err
	}

	log.Info("Certificate successfully reconciled", "operation", op)
	return nil
}

// SetupWithManager initializes controller manager
func (r *HTTPProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	listHPs := handler.ToRequestsFunc(
		func(a handler.MapObject) []reconcile.Request {
			if a.Meta.GetNamespace() != r.ServiceKey.Namespace {
				return nil
			}
			if a.Meta.GetName() != r.ServiceKey.Name {
				return nil
			}

			ctx := context.Background()
			var hpList projectcontourv1.HTTPProxyList
			err := r.List(ctx, &hpList)
			if err != nil {
				r.Log.Error(err, "listing HTTPProxy failed")
				return nil
			}

			requests := make([]reconcile.Request, len(hpList.Items))
			for i, hp := range hpList.Items {
				requests[i] = reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      hp.Name,
					Namespace: hp.Namespace,
				}}
			}
			return requests
		})

	b := ctrl.NewControllerManagedBy(mgr).
		For(&projectcontourv1.HTTPProxy{}).
		Watches(&source.Kind{Type: &corev1.Service{}}, &handler.EnqueueRequestsFromMapFunc{ToRequests: listHPs})
	if r.CreateDNSEndpoint {
		b = b.Owns(&endpoint.DNSEndpoint{})
	}
	if r.CreateCertificate {
		b = b.Owns(&certmanagerv1alpha2.Certificate{})
	}
	return b.Complete(r)
}
