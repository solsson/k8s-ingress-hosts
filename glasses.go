package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	k8sHostname   string
	versionUrl    = "https://github.com/YoleanAgents/k8s-ingress-hosts"
	version       = "dev"
	hostFile      = flag.String("host-file", "/etc/hosts", "host file location")
	writeHostFile = flag.Bool("write", false, "rewrite host file?")
	showVersion   = flag.Bool("version", false, "show version and exit")
	kubeconfig    = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
)

const (
	sectionStart = "# generated using k8s-ingress-hosts start #"
	sectionEnd   = "# generated using k8s-ingress-hosts end #\n"
)

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}

	return os.Getenv("USERPROFILE")
}

type Rule struct {
	Domain  string
	Address string
	Service string
}

func (r *Rule) String() string {
	return fmt.Sprintf("%s\t%s\t# %s", r.Address, r.Domain, r.Service)
}

type HostsList []Rule

func (h HostsList) Len() int      { return len(h) }
func (h HostsList) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h HostsList) Less(i, j int) bool {
	return strings.ToLower(h[i].Domain) < strings.ToLower(h[j].Domain)
}

func k8sHost(config *rest.Config) string {
	u, err := url.Parse(config.Host)
	if err != nil {
		log.Fatalln(err.Error())
	}

	return u.Hostname()
}

func tryWriteToHostFile(hostEntries string) error {

	block := []byte(fmt.Sprintf("%s\n%s\n%s", sectionStart, hostEntries, sectionEnd))
	fileContent, err := os.ReadFile(*hostFile)
	if err != nil {
		return err
	}

	re := regexp.MustCompile(fmt.Sprintf("(?ms)%s(.*)%s", sectionStart, sectionEnd))
	if re.Match(fileContent) {
		fileContent = re.ReplaceAll(fileContent, block)
	} else {
		fileContent = append(fileContent, block...)
	}

	if err := os.WriteFile(*hostFile, fileContent, 0644); err != nil {
		return err
	}

	fmt.Println(hostEntries)
	return nil
}

// gatewayAddress looks up a Gateway resource and returns its first address from status
func gatewayAddress(dynClient dynamic.Interface, namespace, name string) string {
	gatewayGVR := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}

	gw, err := dynClient.Resource(gatewayGVR).Namespace(namespace).Get(context.TODO(), name, metaV1.GetOptions{})
	if err != nil {
		return ""
	}

	status, ok := gw.Object["status"].(map[string]interface{})
	if !ok {
		return ""
	}

	addresses, ok := status["addresses"].([]interface{})
	if !ok {
		return ""
	}

	for _, addr := range addresses {
		addrMap, ok := addr.(map[string]interface{})
		if !ok {
			continue
		}
		if value, ok := addrMap["value"].(string); ok {
			return value
		}
	}

	return ""
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Printf("k8s-ingress-hosts\n url: %s\n version: %s\n", versionUrl, version)
		os.Exit(0)
	}

	fmt.Println("# reading k8s ingress resources...")
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatalln(err.Error())
	}

	k8sHostname = k8sHost(config)

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalln(err.Error())
	}

	var entries HostsList

	// Collect from Ingress resources
	ingress, err := client.NetworkingV1().Ingresses("").List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		log.Fatalln(err.Error())
	}

	for _, elem := range ingress.Items {
		// Determine the address from ingress status
		address := k8sHostname
		for _, lb := range elem.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				address = lb.IP
			} else if lb.Hostname != "" {
				address = lb.Hostname
			}
		}

		for _, rule := range elem.Spec.Rules {
			entries = append(entries, Rule{
				Domain:  rule.Host,
				Address: address,
				Service: elem.Name,
			})
		}
	}

	// Collect from Gateway API HTTPRoute resources
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalln(err.Error())
	}

	httpRouteGVR := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}

	routes, err := dynClient.Resource(httpRouteGVR).Namespace("").List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		// Gateway API may not be installed, skip silently
		fmt.Fprintf(os.Stderr, "# note: could not list HTTPRoutes: %v\n", err)
	} else {
		// Cache gateway addresses to avoid repeated lookups
		gwAddressCache := make(map[string]string)

		for _, route := range routes.Items {
			routeName := route.GetName()
			routeNamespace := route.GetNamespace()

			spec, ok := route.Object["spec"].(map[string]interface{})
			if !ok {
				continue
			}

			// Get hostnames from the HTTPRoute
			hostnames, _ := spec["hostnames"].([]interface{})

			// Resolve address from parent Gateway refs
			address := k8sHostname
			parentRefs, _ := spec["parentRefs"].([]interface{})
			for _, ref := range parentRefs {
				refMap, ok := ref.(map[string]interface{})
				if !ok {
					continue
				}
				gwName, _ := refMap["name"].(string)
				gwNamespace, _ := refMap["namespace"].(string)
				if gwNamespace == "" {
					gwNamespace = routeNamespace
				}

				cacheKey := gwNamespace + "/" + gwName
				if cached, ok := gwAddressCache[cacheKey]; ok {
					if cached != "" {
						address = cached
					}
				} else {
					addr := gatewayAddress(dynClient, gwNamespace, gwName)
					gwAddressCache[cacheKey] = addr
					if addr != "" {
						address = addr
					}
				}
			}

			for _, h := range hostnames {
				hostname, ok := h.(string)
				if !ok {
					continue
				}
				entries = append(entries, Rule{
					Domain:  hostname,
					Address: address,
					Service: fmt.Sprintf("%s/%s", routeNamespace, routeName),
				})
			}
		}
	}

	sort.Sort(HostsList(entries))

	var hostEntries string
	for _, item := range entries {
		hostEntries = hostEntries + fmt.Sprintf("%s\n", item.String())
	}

	wBuffer := new(bytes.Buffer)
	writer := tabwriter.NewWriter(wBuffer, 0, 0, 2, ' ', 0)
	fmt.Fprint(writer, hostEntries)
	writer.Flush()

	if !*writeHostFile {
		fmt.Println(wBuffer.String())
		os.Exit(0)
	}

	if err := tryWriteToHostFile(wBuffer.String()); err != nil {
		log.Fatalln(err)
	}

}
