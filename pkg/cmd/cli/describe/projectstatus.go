package describe

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	kapi "k8s.io/kubernetes/pkg/api"
	kapierrors "k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	utilerrors "k8s.io/kubernetes/pkg/util/errors"
	"k8s.io/kubernetes/pkg/util/sets"

	osgraph "github.com/openshift/origin/pkg/api/graph"
	"github.com/openshift/origin/pkg/api/graph/graphview"
	kubeedges "github.com/openshift/origin/pkg/api/kubegraph"
	kubeanalysis "github.com/openshift/origin/pkg/api/kubegraph/analysis"
	kubegraph "github.com/openshift/origin/pkg/api/kubegraph/nodes"
	buildapi "github.com/openshift/origin/pkg/build/api"
	buildedges "github.com/openshift/origin/pkg/build/graph"
	buildanalysis "github.com/openshift/origin/pkg/build/graph/analysis"
	buildgraph "github.com/openshift/origin/pkg/build/graph/nodes"
	"github.com/openshift/origin/pkg/client"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	deployedges "github.com/openshift/origin/pkg/deploy/graph"
	deployanalysis "github.com/openshift/origin/pkg/deploy/graph/analysis"
	deploygraph "github.com/openshift/origin/pkg/deploy/graph/nodes"
	deployutil "github.com/openshift/origin/pkg/deploy/util"
	imageapi "github.com/openshift/origin/pkg/image/api"
	imageedges "github.com/openshift/origin/pkg/image/graph"
	imagegraph "github.com/openshift/origin/pkg/image/graph/nodes"
	projectapi "github.com/openshift/origin/pkg/project/api"
	routeapi "github.com/openshift/origin/pkg/route/api"
	routeedges "github.com/openshift/origin/pkg/route/graph"
	routeanalysis "github.com/openshift/origin/pkg/route/graph/analysis"
	routegraph "github.com/openshift/origin/pkg/route/graph/nodes"
	"github.com/openshift/origin/pkg/util/errors"
	"github.com/openshift/origin/pkg/util/parallel"
)

const ForbiddenListWarning = "Forbidden"

// ProjectStatusDescriber generates extended information about a Project
type ProjectStatusDescriber struct {
	K       kclient.Interface
	C       client.Interface
	Server  string
	Suggest bool

	LogsCommandName             string
	SecurityPolicyCommandFormat string
	SetProbeCommandName         string
}

func (d *ProjectStatusDescriber) MakeGraph(namespace string) (osgraph.Graph, sets.String, error) {
	g := osgraph.New()

	loaders := []GraphLoader{
		&serviceLoader{namespace: namespace, lister: d.K},
		&serviceAccountLoader{namespace: namespace, lister: d.K},
		&secretLoader{namespace: namespace, lister: d.K},
		&rcLoader{namespace: namespace, lister: d.K},
		&podLoader{namespace: namespace, lister: d.K},
		// TODO check swagger for feature enablement and selectively add bcLoader and buildLoader
		// then remove errors.TolerateNotFoundError method.
		&bcLoader{namespace: namespace, lister: d.C},
		&buildLoader{namespace: namespace, lister: d.C},
		&isLoader{namespace: namespace, lister: d.C},
		&dcLoader{namespace: namespace, lister: d.C},
		&routeLoader{namespace: namespace, lister: d.C},
	}
	loadingFuncs := []func() error{}
	for _, loader := range loaders {
		loadingFuncs = append(loadingFuncs, loader.Load)
	}

	forbiddenResources := sets.String{}
	if errs := parallel.Run(loadingFuncs...); len(errs) > 0 {
		actualErrors := []error{}
		for _, err := range errs {
			if kapierrors.IsForbidden(err) {
				forbiddenErr := err.(*kapierrors.StatusError)
				if (forbiddenErr.Status().Details != nil) && (len(forbiddenErr.Status().Details.Kind) > 0) {
					forbiddenResources.Insert(forbiddenErr.Status().Details.Kind)
				}
				continue
			}
			actualErrors = append(actualErrors, err)
		}

		if len(actualErrors) > 0 {
			return g, forbiddenResources, utilerrors.NewAggregate(actualErrors)
		}
	}

	for _, loader := range loaders {
		loader.AddToGraph(g)
	}

	kubeedges.AddAllExposedPodTemplateSpecEdges(g)
	kubeedges.AddAllExposedPodEdges(g)
	kubeedges.AddAllManagedByRCPodEdges(g)
	kubeedges.AddAllRequestedServiceAccountEdges(g)
	kubeedges.AddAllMountableSecretEdges(g)
	kubeedges.AddAllMountedSecretEdges(g)
	buildedges.AddAllInputOutputEdges(g)
	buildedges.AddAllBuildEdges(g)
	deployedges.AddAllTriggerEdges(g)
	deployedges.AddAllDeploymentEdges(g)
	imageedges.AddAllImageStreamRefEdges(g)
	imageedges.AddAllImageStreamImageRefEdges(g)
	routeedges.AddAllRouteEdges(g)

	return g, forbiddenResources, nil
}

// Describe returns the description of a project
func (d *ProjectStatusDescriber) Describe(namespace, name string) (string, error) {
	var f formatter = namespacedFormatter{}

	g, forbiddenResources, err := d.MakeGraph(namespace)
	if err != nil {
		return "", err
	}

	allNamespaces := namespace == kapi.NamespaceAll
	var project *projectapi.Project
	if !allNamespaces {
		p, err := d.C.Projects().Get(namespace)
		if err != nil {
			return "", err
		}
		project = p
		f = namespacedFormatter{currentNamespace: namespace}
	}

	coveredNodes := graphview.IntSet{}

	services, coveredByServices := graphview.AllServiceGroups(g, coveredNodes)
	coveredNodes.Insert(coveredByServices.List()...)

	standaloneDCs, coveredByDCs := graphview.AllDeploymentConfigPipelines(g, coveredNodes)
	coveredNodes.Insert(coveredByDCs.List()...)

	standaloneRCs, coveredByRCs := graphview.AllReplicationControllers(g, coveredNodes)
	coveredNodes.Insert(coveredByRCs.List()...)

	standaloneImages, coveredByImages := graphview.AllImagePipelinesFromBuildConfig(g, coveredNodes)
	coveredNodes.Insert(coveredByImages.List()...)

	standalonePods, coveredByPods := graphview.AllPods(g, coveredNodes)
	coveredNodes.Insert(coveredByPods.List()...)

	return tabbedString(func(out *tabwriter.Writer) error {
		indent := "  "
		if allNamespaces {
			fmt.Fprintf(out, describeAllProjectsOnServer(f, d.Server))
		} else {
			fmt.Fprintf(out, describeProjectAndServer(f, project, d.Server))
		}

		for _, service := range services {
			if !service.Service.Found() {
				continue
			}
			local := namespacedFormatter{currentNamespace: service.Service.Namespace}

			var exposes []string
			for _, routeNode := range service.ExposingRoutes {
				exposes = append(exposes, describeRouteInServiceGroup(local, routeNode)...)
			}
			sort.Sort(exposedRoutes(exposes))

			fmt.Fprintln(out)
			printLines(out, "", 0, describeServiceInServiceGroup(f, service, exposes...)...)

			for _, dcPipeline := range service.DeploymentConfigPipelines {
				printLines(out, indent, 1, describeDeploymentInServiceGroup(local, dcPipeline)...)
			}

		rcNode:
			for _, rcNode := range service.FulfillingRCs {
				for _, coveredDC := range service.FulfillingDCs {
					if deployedges.BelongsToDeploymentConfig(coveredDC.DeploymentConfig, rcNode.ReplicationController) {
						continue rcNode
					}
				}
				printLines(out, indent, 1, describeRCInServiceGroup(local, rcNode)...)
			}

		pod:
			for _, podNode := range service.FulfillingPods {
				// skip pods that have been displayed in a roll-up of RCs and DCs (by implicit usage of RCs)
				for _, coveredRC := range service.FulfillingRCs {
					if g.Edge(podNode, coveredRC) != nil {
						continue pod
					}
				}
				printLines(out, indent, 1, describePodInServiceGroup(local, podNode)...)
			}
		}

		for _, standaloneDC := range standaloneDCs {
			fmt.Fprintln(out)
			printLines(out, indent, 0, describeDeploymentInServiceGroup(f, standaloneDC)...)
		}

		for _, standaloneImage := range standaloneImages {
			fmt.Fprintln(out)
			lines := describeStandaloneBuildGroup(f, standaloneImage, namespace)
			lines = append(lines, describeAdditionalBuildDetail(standaloneImage.Build, standaloneImage.LastSuccessfulBuild, standaloneImage.LastUnsuccessfulBuild, standaloneImage.ActiveBuilds, standaloneImage.DestinationResolved, true)...)
			printLines(out, indent, 0, lines...)
		}

		for _, standaloneRC := range standaloneRCs {
			fmt.Fprintln(out)
			printLines(out, indent, 0, describeRCInServiceGroup(f, standaloneRC.RC)...)
		}

		monopods, err := filterBoringPods(standalonePods)
		if err != nil {
			return err
		}
		for _, monopod := range monopods {
			fmt.Fprintln(out)
			printLines(out, indent, 0, describeMonopod(f, monopod.Pod)...)
		}

		allMarkers := osgraph.Markers{}
		allMarkers = append(allMarkers, createForbiddenMarkers(forbiddenResources)...)
		for _, scanner := range getMarkerScanners(d.LogsCommandName, d.SecurityPolicyCommandFormat, d.SetProbeCommandName) {
			allMarkers = append(allMarkers, scanner(g, f)...)
		}

		// TODO: Provide an option to chase these hidden markers.
		allMarkers = allMarkers.FilterByNamespace(namespace)

		fmt.Fprintln(out)

		sort.Stable(osgraph.ByKey(allMarkers))
		sort.Stable(osgraph.ByNodeID(allMarkers))

		errorMarkers := allMarkers.BySeverity(osgraph.ErrorSeverity)
		errorSuggestions := 0
		if len(errorMarkers) > 0 {
			fmt.Fprintln(out, "Errors:")
			for _, marker := range errorMarkers {
				fmt.Fprintln(out, indent+"* "+marker.Message)
				if len(marker.Suggestion) > 0 {
					errorSuggestions++
					if d.Suggest {
						switch s := marker.Suggestion.String(); {
						case strings.Contains(s, "\n"):
							fmt.Fprintln(out)
							for _, line := range strings.Split(s, "\n") {
								fmt.Fprintln(out, indent+"  "+line)
							}
						case len(s) > 0:
							fmt.Fprintln(out, indent+"  try: "+s)
						}
					}
				}
			}
		}

		warningMarkers := allMarkers.BySeverity(osgraph.WarningSeverity)
		if len(warningMarkers) > 0 {
			if d.Suggest {
				fmt.Fprintln(out, "Warnings:")
			}
			for _, marker := range warningMarkers {
				if d.Suggest {
					fmt.Fprintln(out, indent+"* "+marker.Message)
					switch s := marker.Suggestion.String(); {
					case strings.Contains(s, "\n"):
						fmt.Fprintln(out)
						for _, line := range strings.Split(s, "\n") {
							fmt.Fprintln(out, indent+"  "+line)
						}
					case len(s) > 0:
						fmt.Fprintln(out, indent+"  try: "+s)
					}
				}
			}
		}

		// We print errors by default and warnings if -v is used. If we get none,
		// this would be an extra new line.
		if len(errorMarkers) != 0 || (d.Suggest && len(warningMarkers) != 0) {
			fmt.Fprintln(out)
		}

		errors, warnings := "", ""
		if len(errorMarkers) == 1 {
			errors = "1 error"
		} else if len(errorMarkers) > 1 {
			errors = fmt.Sprintf("%d errors", len(errorMarkers))
		}
		if len(warningMarkers) == 1 {
			warnings = "1 warning"
		} else if len(warningMarkers) > 1 {
			warnings = fmt.Sprintf("%d warnings", len(warningMarkers))
		}

		switch {
		case !d.Suggest && len(errorMarkers) > 0 && len(warningMarkers) > 0:
			fmt.Fprintf(out, "%s and %s identified, use 'oc status -v' to see details.\n", errors, warnings)

		case !d.Suggest && len(errorMarkers) > 0 && errorSuggestions > 0:
			fmt.Fprintf(out, "%s identified, use 'oc status -v' to see details.\n", errors)

		case !d.Suggest && len(warningMarkers) > 0:
			fmt.Fprintf(out, "%s identified, use 'oc status -v' to see details.\n", warnings)

		case (len(services) == 0) && (len(standaloneDCs) == 0) && (len(standaloneImages) == 0):
			fmt.Fprintln(out, "You have no services, deployment configs, or build configs.")
			fmt.Fprintln(out, "Run 'oc new-app' to create an application.")

		default:
			fmt.Fprintln(out, "View details with 'oc describe <resource>/<name>' or list everything with 'oc get all'.")
		}

		return nil
	})
}

func createForbiddenMarkers(forbiddenResources sets.String) []osgraph.Marker {
	markers := []osgraph.Marker{}
	for forbiddenResource := range forbiddenResources {
		markers = append(markers, osgraph.Marker{
			Severity: osgraph.WarningSeverity,
			Key:      ForbiddenListWarning,
			Message:  fmt.Sprintf("Unable to list %s resources.  Not all status relationships can be established.", forbiddenResource),
		})
	}
	return markers
}

func getMarkerScanners(logsCommandName, securityPolicyCommandFormat, setProbeCommandName string) []osgraph.MarkerScanner {
	return []osgraph.MarkerScanner{
		func(g osgraph.Graph, f osgraph.Namer) []osgraph.Marker {
			return kubeanalysis.FindRestartingPods(g, f, logsCommandName, securityPolicyCommandFormat)
		},
		kubeanalysis.FindDuelingReplicationControllers,
		kubeanalysis.FindMissingSecrets,
		buildanalysis.FindUnpushableBuildConfigs,
		buildanalysis.FindCircularBuilds,
		buildanalysis.FindPendingTags,
		deployanalysis.FindDeploymentConfigTriggerErrors,
		buildanalysis.FindMissingInputImageStreams,
		func(g osgraph.Graph, f osgraph.Namer) []osgraph.Marker {
			return deployanalysis.FindDeploymentConfigReadinessWarnings(g, f, setProbeCommandName)
		},
		routeanalysis.FindPortMappingIssues,
		routeanalysis.FindMissingTLSTerminationType,
		routeanalysis.FindPathBasedPassthroughRoutes,
		routeanalysis.FindRouteAdmissionFailures,
		routeanalysis.FindMissingRouter,
		// We disable this feature by default and we don't have a capability detection for this sort of thing.  Disable this check for now.
		// kubeanalysis.FindUnmountableSecrets,
	}
}

func printLines(out io.Writer, indent string, depth int, lines ...string) {
	for i, s := range lines {
		fmt.Fprintf(out, strings.Repeat(indent, depth))
		if i != 0 {
			fmt.Fprint(out, indent)
		}
		fmt.Fprintln(out, s)
	}
}

func indentLines(indent string, lines ...string) []string {
	ret := make([]string, 0, len(lines))
	for _, line := range lines {
		ret = append(ret, indent+line)
	}

	return ret
}

type formatter interface {
	ResourceName(obj interface{}) string
}

func namespaceNameWithType(resource, name, namespace, defaultNamespace string, noNamespace bool) string {
	if noNamespace || namespace == defaultNamespace || len(namespace) == 0 {
		return resource + "/" + name
	}
	return resource + "/" + name + "[" + namespace + "]"
}

var namespaced = namespacedFormatter{}

type namespacedFormatter struct {
	hideNamespace    bool
	currentNamespace string
}

func (f namespacedFormatter) ResourceName(obj interface{}) string {
	switch t := obj.(type) {

	case *kubegraph.PodNode:
		return namespaceNameWithType("pod", t.Name, t.Namespace, f.currentNamespace, f.hideNamespace)
	case *kubegraph.ServiceNode:
		return namespaceNameWithType("svc", t.Name, t.Namespace, f.currentNamespace, f.hideNamespace)
	case *kubegraph.SecretNode:
		return namespaceNameWithType("secret", t.Name, t.Namespace, f.currentNamespace, f.hideNamespace)
	case *kubegraph.ServiceAccountNode:
		return namespaceNameWithType("sa", t.Name, t.Namespace, f.currentNamespace, f.hideNamespace)
	case *kubegraph.ReplicationControllerNode:
		return namespaceNameWithType("rc", t.Name, t.Namespace, f.currentNamespace, f.hideNamespace)

	case *imagegraph.ImageStreamNode:
		return namespaceNameWithType("is", t.ImageStream.Name, t.ImageStream.Namespace, f.currentNamespace, f.hideNamespace)
	case *imagegraph.ImageStreamTagNode:
		return namespaceNameWithType("istag", t.ImageStreamTag.Name, t.ImageStreamTag.Namespace, f.currentNamespace, f.hideNamespace)
	case *imagegraph.ImageStreamImageNode:
		return namespaceNameWithType("isi", t.ImageStreamImage.Name, t.ImageStreamImage.Namespace, f.currentNamespace, f.hideNamespace)
	case *imagegraph.ImageNode:
		return namespaceNameWithType("image", t.Image.Name, t.Image.Namespace, f.currentNamespace, f.hideNamespace)
	case *buildgraph.BuildConfigNode:
		return namespaceNameWithType("bc", t.BuildConfig.Name, t.BuildConfig.Namespace, f.currentNamespace, f.hideNamespace)
	case *buildgraph.BuildNode:
		return namespaceNameWithType("build", t.Build.Name, t.Build.Namespace, f.currentNamespace, f.hideNamespace)

	case *deploygraph.DeploymentConfigNode:
		return namespaceNameWithType("dc", t.DeploymentConfig.Name, t.DeploymentConfig.Namespace, f.currentNamespace, f.hideNamespace)

	case *routegraph.RouteNode:
		return namespaceNameWithType("route", t.Route.Name, t.Route.Namespace, f.currentNamespace, f.hideNamespace)

	default:
		return fmt.Sprintf("<unrecognized object: %#v>", obj)
	}
}

func describeProjectAndServer(f formatter, project *projectapi.Project, server string) string {
	if len(server) == 0 {
		return fmt.Sprintf("In project %s on server %s\n", projectapi.DisplayNameAndNameForProject(project), server)
	}
	return fmt.Sprintf("In project %s on server %s\n", projectapi.DisplayNameAndNameForProject(project), server)

}

func describeAllProjectsOnServer(f formatter, server string) string {
	if len(server) == 0 {
		return "Showing all projects\n"
	}
	return fmt.Sprintf("Showing all projects on server %s\n", server)
}

func describeDeploymentInServiceGroup(f formatter, deploy graphview.DeploymentConfigPipeline) []string {
	local := namespacedFormatter{currentNamespace: deploy.Deployment.Namespace}

	includeLastPass := deploy.ActiveDeployment == nil
	if len(deploy.Images) == 1 {
		format := "%s deploys %s %s"
		if deploy.Deployment.Spec.Test {
			format = "%s test deploys %s %s"
		}
		lines := []string{fmt.Sprintf(format, f.ResourceName(deploy.Deployment), describeImageInPipeline(local, deploy.Images[0], deploy.Deployment.Namespace), describeDeploymentConfigTrigger(deploy.Deployment.DeploymentConfig))}
		if len(lines[0]) > 120 && strings.Contains(lines[0], " <- ") {
			segments := strings.SplitN(lines[0], " <- ", 2)
			lines[0] = segments[0] + " <-"
			lines = append(lines, segments[1])
		}
		lines = append(lines, indentLines("  ", describeAdditionalBuildDetail(deploy.Images[0].Build, deploy.Images[0].LastSuccessfulBuild, deploy.Images[0].LastUnsuccessfulBuild, deploy.Images[0].ActiveBuilds, deploy.Images[0].DestinationResolved, includeLastPass)...)...)
		lines = append(lines, describeDeployments(local, deploy.Deployment, deploy.ActiveDeployment, deploy.InactiveDeployments, 3)...)
		return lines
	}

	format := "%s deploys %s"
	if deploy.Deployment.Spec.Test {
		format = "%s test deploys %s"
	}
	lines := []string{fmt.Sprintf(format, f.ResourceName(deploy.Deployment), describeDeploymentConfigTrigger(deploy.Deployment.DeploymentConfig))}
	for _, image := range deploy.Images {
		lines = append(lines, describeImageInPipeline(local, image, deploy.Deployment.Namespace))
		lines = append(lines, indentLines("  ", describeAdditionalBuildDetail(image.Build, image.LastSuccessfulBuild, image.LastUnsuccessfulBuild, image.ActiveBuilds, image.DestinationResolved, includeLastPass)...)...)
		lines = append(lines, describeDeployments(local, deploy.Deployment, deploy.ActiveDeployment, deploy.InactiveDeployments, 3)...)
	}
	return lines
}

func describeRCInServiceGroup(f formatter, rcNode *kubegraph.ReplicationControllerNode) []string {
	if rcNode.ReplicationController.Spec.Template == nil {
		return []string{}
	}

	images := []string{}
	for _, container := range rcNode.ReplicationController.Spec.Template.Spec.Containers {
		images = append(images, container.Image)
	}

	lines := []string{fmt.Sprintf("%s runs %s", f.ResourceName(rcNode), strings.Join(images, ", "))}
	lines = append(lines, describeRCStatus(rcNode.ReplicationController))

	return lines
}

func describePodInServiceGroup(f formatter, podNode *kubegraph.PodNode) []string {
	images := []string{}
	for _, container := range podNode.Pod.Spec.Containers {
		images = append(images, container.Image)
	}

	lines := []string{fmt.Sprintf("%s runs %s", f.ResourceName(podNode), strings.Join(images, ", "))}
	return lines
}

func describeMonopod(f formatter, podNode *kubegraph.PodNode) []string {
	images := []string{}
	for _, container := range podNode.Pod.Spec.Containers {
		images = append(images, container.Image)
	}

	lines := []string{fmt.Sprintf("%s runs %s", f.ResourceName(podNode), strings.Join(images, ", "))}
	return lines
}

// exposedRoutes orders strings by their leading prefix (https:// -> http:// other prefixes), then by
// the shortest distance up to the first space (indicating a break), then alphabetically:
//
//   https://test.com
//   https://www.test.com
//   http://t.com
//   other string
//
type exposedRoutes []string

func (e exposedRoutes) Len() int      { return len(e) }
func (e exposedRoutes) Swap(i, j int) { e[i], e[j] = e[j], e[i] }
func (e exposedRoutes) Less(i, j int) bool {
	a, b := e[i], e[j]
	prefixA, prefixB := strings.HasPrefix(a, "https://"), strings.HasPrefix(b, "https://")
	switch {
	case prefixA && !prefixB:
		return true
	case !prefixA && prefixB:
		return false
	case !prefixA && !prefixB:
		prefixA, prefixB = strings.HasPrefix(a, "http://"), strings.HasPrefix(b, "http://")
		switch {
		case prefixA && !prefixB:
			return true
		case !prefixA && prefixB:
			return false
		case !prefixA && !prefixB:
			return a < b
		default:
			a, b = a[7:], b[7:]
		}
	default:
		a, b = a[8:], b[8:]
	}
	lA, lB := strings.Index(a, " "), strings.Index(b, " ")
	if lA == -1 {
		lA = len(a)
	}
	if lB == -1 {
		lB = len(b)
	}
	switch {
	case lA < lB:
		return true
	case lA > lB:
		return false
	default:
		return a < b
	}
}

func extractRouteInfo(route *routeapi.Route) (requested bool, other []string, errors []string) {
	reasons := sets.NewString()
	for _, ingress := range route.Status.Ingress {
		exact := route.Spec.Host == ingress.Host
		switch status, condition := routeapi.IngressConditionStatus(&ingress, routeapi.RouteAdmitted); status {
		case kapi.ConditionFalse:
			reasons.Insert(condition.Reason)
		default:
			if exact {
				requested = true
			} else {
				other = append(other, ingress.Host)
			}
		}
	}
	return requested, other, reasons.List()
}

func describeRouteExposed(host string, route *routeapi.Route, errors bool) string {
	var trailer string
	if errors {
		trailer = " (!)"
	}
	var prefix string
	switch {
	case route.Spec.TLS == nil:
		prefix = fmt.Sprintf("http://%s", host)
	case route.Spec.TLS.Termination == routeapi.TLSTerminationPassthrough:
		prefix = fmt.Sprintf("https://%s (passthrough)", host)
	case route.Spec.TLS.Termination == routeapi.TLSTerminationReencrypt:
		prefix = fmt.Sprintf("https://%s (reencrypt)", host)
	case route.Spec.TLS.Termination != routeapi.TLSTerminationEdge:
		// future proof against other types of TLS termination being added
		prefix = fmt.Sprintf("https://%s", host)
	case route.Spec.TLS.InsecureEdgeTerminationPolicy == routeapi.InsecureEdgeTerminationPolicyRedirect:
		prefix = fmt.Sprintf("https://%s (redirects)", host)
	case route.Spec.TLS.InsecureEdgeTerminationPolicy == routeapi.InsecureEdgeTerminationPolicyAllow:
		prefix = fmt.Sprintf("https://%s (and http)", host)
	default:
		prefix = fmt.Sprintf("https://%s", host)
	}

	if route.Spec.Port != nil && len(route.Spec.Port.TargetPort.String()) > 0 {
		return fmt.Sprintf("%s to pod port %s%s", prefix, route.Spec.Port.TargetPort.String(), trailer)
	}
	return fmt.Sprintf("%s%s", prefix, trailer)
}

func describeRouteInServiceGroup(f formatter, routeNode *routegraph.RouteNode) []string {
	// markers should cover printing information about admission failure
	requested, other, errors := extractRouteInfo(routeNode.Route)
	var lines []string
	if requested {
		lines = append(lines, describeRouteExposed(routeNode.Spec.Host, routeNode.Route, len(errors) > 0))
	}
	for _, s := range other {
		lines = append(lines, describeRouteExposed(s, routeNode.Route, len(errors) > 0))
	}
	if len(lines) == 0 {
		switch {
		case len(errors) >= 1:
			// router rejected the output
			lines = append(lines, fmt.Sprintf("%s not accepted: %s", f.ResourceName(routeNode), errors[0]))
		case len(routeNode.Spec.Host) == 0:
			// no errors or output, likely no router running and no default domain
			lines = append(lines, fmt.Sprintf("%s has no host set", f.ResourceName(routeNode)))
		case len(routeNode.Status.Ingress) == 0:
			// host set, but no ingress, an older legacy router
			lines = append(lines, describeRouteExposed(routeNode.Spec.Host, routeNode.Route, false))
		default:
			// multiple conditions but no host exposed, use the generic legacy output
			lines = append(lines, fmt.Sprintf("exposed as %s by %s", routeNode.Spec.Host, f.ResourceName(routeNode)))
		}
	}
	return lines
}

func describeDeploymentConfigTrigger(dc *deployapi.DeploymentConfig) string {
	if len(dc.Spec.Triggers) == 0 {
		return "(manual)"
	}

	return ""
}

func describeStandaloneBuildGroup(f formatter, pipeline graphview.ImagePipeline, namespace string) []string {
	switch {
	case pipeline.Build != nil:
		lines := []string{describeBuildInPipeline(f, pipeline.Build.BuildConfig, pipeline.BaseImage)}
		if pipeline.Image != nil {
			lines = append(lines, fmt.Sprintf("pushes to %s", describeImageTagInPipeline(f, pipeline.Image, namespace)))
		}
		return lines
	case pipeline.Image != nil:
		return []string{describeImageTagInPipeline(f, pipeline.Image, namespace)}
	default:
		return []string{"<unknown>"}
	}
}

func describeImageInPipeline(f formatter, pipeline graphview.ImagePipeline, namespace string) string {
	switch {
	case pipeline.Image != nil && pipeline.Build != nil:
		return fmt.Sprintf("%s <- %s", describeImageTagInPipeline(f, pipeline.Image, namespace), describeBuildInPipeline(f, pipeline.Build.BuildConfig, pipeline.BaseImage))
	case pipeline.Image != nil:
		return describeImageTagInPipeline(f, pipeline.Image, namespace)
	case pipeline.Build != nil:
		return describeBuildInPipeline(f, pipeline.Build.BuildConfig, pipeline.BaseImage)
	default:
		return "<unknown>"
	}
}

func describeImageTagInPipeline(f formatter, image graphview.ImageTagLocation, namespace string) string {
	switch t := image.(type) {
	case *imagegraph.ImageStreamTagNode:
		if t.ImageStreamTag.Namespace != namespace {
			return image.ImageSpec()
		}
		return f.ResourceName(t)
	default:
		return image.ImageSpec()
	}
}

func describeBuildInPipeline(f formatter, build *buildapi.BuildConfig, baseImage graphview.ImageTagLocation) string {
	switch {
	case build.Spec.Strategy.DockerStrategy != nil:
		// TODO: handle case where no source repo
		source, ok := describeSourceInPipeline(&build.Spec.Source)
		if !ok {
			return fmt.Sprintf("bc/%s unconfigured docker build - no source set", build.Name)
		}
		return fmt.Sprintf("bc/%s docker build of %s", build.Name, source)
	case build.Spec.Strategy.SourceStrategy != nil:
		source, ok := describeSourceInPipeline(&build.Spec.Source)
		if !ok {
			return fmt.Sprintf("bc/%s unconfigured source build", build.Name)
		}
		if baseImage == nil {
			return fmt.Sprintf("bc/%s %s; no image set", build.Name, source)
		}
		return fmt.Sprintf("bc/%s builds %s with %s", build.Name, source, baseImage.ImageSpec())
	case build.Spec.Strategy.CustomStrategy != nil:
		source, ok := describeSourceInPipeline(&build.Spec.Source)
		if !ok {
			return fmt.Sprintf("bc/%s custom build ", build.Name)
		}
		return fmt.Sprintf("bc/%s custom build of %s", build.Name, source)
	default:
		return fmt.Sprintf("bc/%s unrecognized build", build.Name)
	}
}

func describeAdditionalBuildDetail(build *buildgraph.BuildConfigNode, lastSuccessfulBuild *buildgraph.BuildNode, lastUnsuccessfulBuild *buildgraph.BuildNode, activeBuilds []*buildgraph.BuildNode, pushTargetResolved bool, includeSuccess bool) []string {
	if build == nil {
		return nil
	}
	out := []string{}

	passTime := unversioned.Time{}
	if lastSuccessfulBuild != nil {
		passTime = buildTimestamp(lastSuccessfulBuild.Build)
	}
	failTime := unversioned.Time{}
	if lastUnsuccessfulBuild != nil {
		failTime = buildTimestamp(lastUnsuccessfulBuild.Build)
	}

	lastTime := failTime
	if passTime.After(failTime.Time) {
		lastTime = passTime
	}

	// display the last successful build if specifically requested or we're going to display an active build for context
	if lastSuccessfulBuild != nil && (includeSuccess || len(activeBuilds) > 0) {
		out = append(out, describeBuildPhase(lastSuccessfulBuild.Build, &passTime, build.BuildConfig.Name, pushTargetResolved))
	}
	if passTime.Before(failTime) {
		out = append(out, describeBuildPhase(lastUnsuccessfulBuild.Build, &failTime, build.BuildConfig.Name, pushTargetResolved))
	}

	if len(activeBuilds) > 0 {
		activeOut := []string{}
		for i := range activeBuilds {
			activeOut = append(activeOut, describeBuildPhase(activeBuilds[i].Build, nil, build.BuildConfig.Name, pushTargetResolved))
		}

		if buildTimestamp(activeBuilds[0].Build).Before(lastTime) {
			out = append(out, activeOut...)
		} else {
			out = append(activeOut, out...)
		}
	}
	if len(out) == 0 && lastSuccessfulBuild == nil {
		out = append(out, "not built yet")
	}
	return out
}

func describeBuildPhase(build *buildapi.Build, t *unversioned.Time, parentName string, pushTargetResolved bool) string {
	imageStreamFailure := ""
	// if we're using an image stream and that image stream is the internal registry and that registry doesn't exist
	if (build.Spec.Output.To != nil) && !pushTargetResolved {
		imageStreamFailure = " (can't push to image)"
	}

	if t == nil {
		ts := buildTimestamp(build)
		t = &ts
	}
	var time string
	if t.IsZero() {
		time = "<unknown>"
	} else {
		time = strings.ToLower(formatRelativeTime(t.Time))
	}
	buildIdentification := fmt.Sprintf("build/%s", build.Name)
	prefix := parentName + "-"
	if strings.HasPrefix(build.Name, prefix) {
		suffix := build.Name[len(prefix):]

		if buildNumber, err := strconv.Atoi(suffix); err == nil {
			buildIdentification = fmt.Sprintf("build #%d", buildNumber)
		}
	}

	revision := describeSourceRevision(build.Spec.Revision)
	if len(revision) != 0 {
		revision = fmt.Sprintf(" - %s", revision)
	}
	switch build.Status.Phase {
	case buildapi.BuildPhaseComplete:
		return fmt.Sprintf("%s succeeded %s ago%s%s", buildIdentification, time, revision, imageStreamFailure)
	case buildapi.BuildPhaseError:
		return fmt.Sprintf("%s stopped with an error %s ago%s%s", buildIdentification, time, revision, imageStreamFailure)
	case buildapi.BuildPhaseFailed:
		return fmt.Sprintf("%s failed %s ago%s%s", buildIdentification, time, revision, imageStreamFailure)
	default:
		status := strings.ToLower(string(build.Status.Phase))
		return fmt.Sprintf("%s %s for %s%s%s", buildIdentification, status, time, revision, imageStreamFailure)
	}
}

func describeSourceRevision(rev *buildapi.SourceRevision) string {
	if rev == nil {
		return ""
	}
	switch {
	case rev.Git != nil:
		author := describeSourceControlUser(rev.Git.Author)
		if len(author) == 0 {
			author = describeSourceControlUser(rev.Git.Committer)
		}
		if len(author) != 0 {
			author = fmt.Sprintf(" (%s)", author)
		}
		commit := rev.Git.Commit
		if len(commit) > 7 {
			commit = commit[:7]
		}
		return fmt.Sprintf("%s: %s%s", commit, rev.Git.Message, author)
	default:
		return ""
	}
}

func describeSourceControlUser(user buildapi.SourceControlUser) string {
	if len(user.Name) == 0 {
		return user.Email
	}
	if len(user.Email) == 0 {
		return user.Name
	}
	return fmt.Sprintf("%s <%s>", user.Name, user.Email)
}

func buildTimestamp(build *buildapi.Build) unversioned.Time {
	if build == nil {
		return unversioned.Time{}
	}
	if !build.Status.CompletionTimestamp.IsZero() {
		return *build.Status.CompletionTimestamp
	}
	if !build.Status.StartTimestamp.IsZero() {
		return *build.Status.StartTimestamp
	}
	return build.CreationTimestamp
}

func describeSourceInPipeline(source *buildapi.BuildSource) (string, bool) {
	switch {
	case source.Git != nil:
		if len(source.Git.Ref) == 0 {
			return source.Git.URI, true
		}
		return fmt.Sprintf("%s#%s", source.Git.URI, source.Git.Ref), true
	case source.Dockerfile != nil:
		return "Dockerfile", true
	}
	return "", false
}

func describeDeployments(f formatter, dcNode *deploygraph.DeploymentConfigNode, activeDeployment *kubegraph.ReplicationControllerNode, inactiveDeployments []*kubegraph.ReplicationControllerNode, count int) []string {
	if dcNode == nil {
		return nil
	}
	out := []string{}
	deploymentsToPrint := append([]*kubegraph.ReplicationControllerNode{}, inactiveDeployments...)

	if activeDeployment == nil {
		on, auto := describeDeploymentConfigTriggers(dcNode.DeploymentConfig)
		if dcNode.DeploymentConfig.Status.LatestVersion == 0 {
			out = append(out, fmt.Sprintf("deployment #1 waiting %s", on))
		} else if auto {
			out = append(out, fmt.Sprintf("deployment #%d pending %s", dcNode.DeploymentConfig.Status.LatestVersion, on))
		}
		// TODO: detect new image available?
	} else {
		deploymentsToPrint = append([]*kubegraph.ReplicationControllerNode{activeDeployment}, inactiveDeployments...)
	}

	for i, deployment := range deploymentsToPrint {
		out = append(out, describeDeploymentStatus(deployment.ReplicationController, i == 0, dcNode.DeploymentConfig.Spec.Test))

		switch {
		case count == -1:
			if deployutil.DeploymentStatusFor(deployment) == deployapi.DeploymentStatusComplete {
				return out
			}
		default:
			if i+1 >= count {
				return out
			}
		}
	}
	return out
}

func describeDeploymentStatus(deploy *kapi.ReplicationController, first, test bool) string {
	timeAt := strings.ToLower(formatRelativeTime(deploy.CreationTimestamp.Time))
	status := deployutil.DeploymentStatusFor(deploy)
	version := deployutil.DeploymentVersionFor(deploy)
	switch status {
	case deployapi.DeploymentStatusFailed:
		reason := deployutil.DeploymentStatusReasonFor(deploy)
		if len(reason) > 0 {
			reason = fmt.Sprintf(": %s", reason)
		}
		// TODO: encode fail time in the rc
		return fmt.Sprintf("deployment #%d failed %s ago%s%s", version, timeAt, reason, describePodSummaryInline(deploy, false))
	case deployapi.DeploymentStatusComplete:
		// TODO: pod status output
		if test {
			return fmt.Sprintf("test deployment #%d deployed %s ago", version, timeAt)
		}
		return fmt.Sprintf("deployment #%d deployed %s ago%s", version, timeAt, describePodSummaryInline(deploy, first))
	case deployapi.DeploymentStatusRunning:
		format := "deployment #%d running for %s%s"
		if test {
			format = "test deployment #%d running for %s%s"
		}
		return fmt.Sprintf(format, version, timeAt, describePodSummaryInline(deploy, false))
	default:
		return fmt.Sprintf("deployment #%d %s %s ago%s", version, strings.ToLower(string(status)), timeAt, describePodSummaryInline(deploy, false))
	}
}

func describeRCStatus(rc *kapi.ReplicationController) string {
	timeAt := strings.ToLower(formatRelativeTime(rc.CreationTimestamp.Time))
	return fmt.Sprintf("rc/%s created %s ago%s", rc.Name, timeAt, describePodSummaryInline(rc, false))
}

func describePodSummaryInline(rc *kapi.ReplicationController, includeEmpty bool) string {
	s := describePodSummary(rc, includeEmpty)
	if len(s) == 0 {
		return s
	}
	change := ""
	desired := rc.Spec.Replicas
	switch {
	case desired < rc.Status.Replicas:
		change = fmt.Sprintf(" reducing to %d", desired)
	case desired > rc.Status.Replicas:
		change = fmt.Sprintf(" growing to %d", desired)
	}
	return fmt.Sprintf(" - %s%s", s, change)
}

func describePodSummary(rc *kapi.ReplicationController, includeEmpty bool) string {
	actual, requested := rc.Status.Replicas, rc.Spec.Replicas
	if actual == requested {
		switch {
		case actual == 0:
			if !includeEmpty {
				return ""
			}
			return "0 pods"
		case actual > 1:
			return fmt.Sprintf("%d pods", actual)
		default:
			return "1 pod"
		}
	}
	return fmt.Sprintf("%d/%d pods", actual, requested)
}

func describeDeploymentConfigTriggers(config *deployapi.DeploymentConfig) (string, bool) {
	hasConfig, hasImage := false, false
	for _, t := range config.Spec.Triggers {
		switch t.Type {
		case deployapi.DeploymentTriggerOnConfigChange:
			hasConfig = true
		case deployapi.DeploymentTriggerOnImageChange:
			hasImage = true
		}
	}
	switch {
	case hasConfig && hasImage:
		return "on image or update", true
	case hasConfig:
		return "on update", true
	case hasImage:
		return "on image", true
	default:
		return "for manual", false
	}
}

func describeServiceInServiceGroup(f formatter, svc graphview.ServiceGroup, exposed ...string) []string {
	spec := svc.Service.Spec
	ip := spec.ClusterIP
	port := describeServicePorts(spec)
	switch {
	case len(exposed) > 1:
		return append([]string{fmt.Sprintf("%s (%s)", exposed[0], f.ResourceName(svc.Service))}, exposed[1:]...)
	case len(exposed) == 1:
		return []string{fmt.Sprintf("%s (%s)", exposed[0], f.ResourceName(svc.Service))}
	case spec.Type == kapi.ServiceTypeNodePort:
		return []string{fmt.Sprintf("%s (all nodes)%s", f.ResourceName(svc.Service), port)}
	case ip == "None":
		return []string{fmt.Sprintf("%s (headless)%s", f.ResourceName(svc.Service), port)}
	case len(ip) == 0:
		return []string{fmt.Sprintf("%s <initializing>%s", f.ResourceName(svc.Service), port)}
	default:
		return []string{fmt.Sprintf("%s - %s%s", f.ResourceName(svc.Service), ip, port)}
	}
}

func portOrNodePort(spec kapi.ServiceSpec, port kapi.ServicePort) string {
	switch {
	case spec.Type != kapi.ServiceTypeNodePort:
		return strconv.Itoa(port.Port)
	case port.NodePort == 0:
		return "<initializing>"
	default:
		return strconv.Itoa(port.NodePort)
	}
}

func describeServicePorts(spec kapi.ServiceSpec) string {
	switch len(spec.Ports) {
	case 0:
		return " no ports"

	case 1:
		port := portOrNodePort(spec, spec.Ports[0])
		if spec.Ports[0].TargetPort.String() == "0" || spec.ClusterIP == kapi.ClusterIPNone || port == spec.Ports[0].TargetPort.String() {
			return fmt.Sprintf(":%s", port)
		}
		return fmt.Sprintf(":%s -> %s", port, spec.Ports[0].TargetPort.String())

	default:
		pairs := []string{}
		for _, port := range spec.Ports {
			externalPort := portOrNodePort(spec, port)
			if port.TargetPort.String() == "0" || spec.ClusterIP == kapi.ClusterIPNone {
				pairs = append(pairs, externalPort)
				continue
			}
			if port.Port == int(port.TargetPort.IntVal) {
				pairs = append(pairs, port.TargetPort.String())
			} else {
				pairs = append(pairs, fmt.Sprintf("%s->%s", externalPort, port.TargetPort.String()))
			}
		}
		return " ports " + strings.Join(pairs, ", ")
	}
}

func filterBoringPods(pods []graphview.Pod) ([]graphview.Pod, error) {
	monopods := []graphview.Pod{}

	for _, pod := range pods {
		actualPod, ok := pod.Pod.Object().(*kapi.Pod)
		if !ok {
			continue
		}
		meta, err := kapi.ObjectMetaFor(actualPod)
		if err != nil {
			return nil, err
		}
		_, isDeployerPod := meta.Labels[deployapi.DeployerPodForDeploymentLabel]
		_, isBuilderPod := meta.Annotations[buildapi.BuildAnnotation]
		isFinished := actualPod.Status.Phase == kapi.PodSucceeded || actualPod.Status.Phase == kapi.PodFailed
		if isDeployerPod || isBuilderPod || isFinished {
			continue
		}
		monopods = append(monopods, pod)
	}

	return monopods, nil
}

// GraphLoader is a stateful interface that provides methods for building the nodes of a graph
type GraphLoader interface {
	// Load is responsible for gathering and saving the objects this GraphLoader should AddToGraph
	Load() error
	// AddToGraph
	AddToGraph(g osgraph.Graph) error
}

type rcLoader struct {
	namespace string
	lister    kclient.ReplicationControllersNamespacer
	items     []kapi.ReplicationController
}

func (l *rcLoader) Load() error {
	list, err := l.lister.ReplicationControllers(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *rcLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		kubegraph.EnsureReplicationControllerNode(g, &l.items[i])
	}

	return nil
}

type serviceLoader struct {
	namespace string
	lister    kclient.ServicesNamespacer
	items     []kapi.Service
}

func (l *serviceLoader) Load() error {
	list, err := l.lister.Services(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *serviceLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		kubegraph.EnsureServiceNode(g, &l.items[i])
	}

	return nil
}

type podLoader struct {
	namespace string
	lister    kclient.PodsNamespacer
	items     []kapi.Pod
}

func (l *podLoader) Load() error {
	list, err := l.lister.Pods(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *podLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		kubegraph.EnsurePodNode(g, &l.items[i])
	}

	return nil
}

type serviceAccountLoader struct {
	namespace string
	lister    kclient.ServiceAccountsNamespacer
	items     []kapi.ServiceAccount
}

func (l *serviceAccountLoader) Load() error {
	list, err := l.lister.ServiceAccounts(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *serviceAccountLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		kubegraph.EnsureServiceAccountNode(g, &l.items[i])
	}

	return nil
}

type secretLoader struct {
	namespace string
	lister    kclient.SecretsNamespacer
	items     []kapi.Secret
}

func (l *secretLoader) Load() error {
	list, err := l.lister.Secrets(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *secretLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		kubegraph.EnsureSecretNode(g, &l.items[i])
	}

	return nil
}

type isLoader struct {
	namespace string
	lister    client.ImageStreamsNamespacer
	items     []imageapi.ImageStream
}

func (l *isLoader) Load() error {
	list, err := l.lister.ImageStreams(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *isLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		imagegraph.EnsureImageStreamNode(g, &l.items[i])
		imagegraph.EnsureAllImageStreamTagNodes(g, &l.items[i])
	}

	return nil
}

type dcLoader struct {
	namespace string
	lister    client.DeploymentConfigsNamespacer
	items     []deployapi.DeploymentConfig
}

func (l *dcLoader) Load() error {
	list, err := l.lister.DeploymentConfigs(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *dcLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		deploygraph.EnsureDeploymentConfigNode(g, &l.items[i])
	}

	return nil
}

type bcLoader struct {
	namespace string
	lister    client.BuildConfigsNamespacer
	items     []buildapi.BuildConfig
}

func (l *bcLoader) Load() error {
	list, err := l.lister.BuildConfigs(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return errors.TolerateNotFoundError(err)
	}

	l.items = list.Items
	return nil
}

func (l *bcLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		buildgraph.EnsureBuildConfigNode(g, &l.items[i])
	}

	return nil
}

type buildLoader struct {
	namespace string
	lister    client.BuildsNamespacer
	items     []buildapi.Build
}

func (l *buildLoader) Load() error {
	list, err := l.lister.Builds(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return errors.TolerateNotFoundError(err)
	}

	l.items = list.Items
	return nil
}

func (l *buildLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		buildgraph.EnsureBuildNode(g, &l.items[i])
	}

	return nil
}

type routeLoader struct {
	namespace string
	lister    client.RoutesNamespacer
	items     []routeapi.Route
}

func (l *routeLoader) Load() error {
	list, err := l.lister.Routes(l.namespace).List(kapi.ListOptions{})
	if err != nil {
		return err
	}

	l.items = list.Items
	return nil
}

func (l *routeLoader) AddToGraph(g osgraph.Graph) error {
	for i := range l.items {
		routegraph.EnsureRouteNode(g, &l.items[i])
	}

	return nil
}
