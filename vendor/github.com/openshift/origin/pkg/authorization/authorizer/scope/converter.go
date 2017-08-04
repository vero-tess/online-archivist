package scope

import (
	"fmt"
	"strings"

	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kutilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	kauthorizer "k8s.io/apiserver/pkg/authorization/authorizer"
	kapi "k8s.io/kubernetes/pkg/api"
	kauthorizationapi "k8s.io/kubernetes/pkg/apis/authorization"

	authorizationapi "github.com/openshift/origin/pkg/authorization/apis/authorization"
	"github.com/openshift/origin/pkg/authorization/authorizer"
	authorizationlister "github.com/openshift/origin/pkg/authorization/generated/listers/authorization/internalversion"
	imageapi "github.com/openshift/origin/pkg/image/apis/image"
	oauthapi "github.com/openshift/origin/pkg/oauth/apis/oauth"
	projectapi "github.com/openshift/origin/pkg/project/apis/project"
	userapi "github.com/openshift/origin/pkg/user/apis/user"
)

// ScopesToRules takes the scopes and return the rules back.  We ALWAYS add the discovery rules and it is possible to get some rules and and
// an error since errors aren't fatal to evaluation
func ScopesToRules(scopes []string, namespace string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]authorizationapi.PolicyRule, error) {
	rules := append([]authorizationapi.PolicyRule{}, authorizationapi.DiscoveryRule)

	errors := []error{}
	for _, scope := range scopes {
		found := false

		for _, evaluator := range ScopeEvaluators {
			if evaluator.Handles(scope) {
				found = true
				currRules, err := evaluator.ResolveRules(scope, namespace, clusterPolicyGetter)
				if err != nil {
					errors = append(errors, err)
					continue
				}

				rules = append(rules, currRules...)
			}
		}

		if !found {
			errors = append(errors, fmt.Errorf("no scope evaluator found for %q", scope))
		}
	}

	return rules, kutilerrors.NewAggregate(errors)
}

// ScopesToVisibleNamespaces returns a list of namespaces that the provided scopes have "get" access to.
// This exists only to support efficiently list/watch of projects (ACLed namespaces)
func ScopesToVisibleNamespaces(scopes []string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) (sets.String, error) {
	if len(scopes) == 0 {
		return sets.NewString("*"), nil
	}

	visibleNamespaces := sets.String{}

	errors := []error{}
	for _, scope := range scopes {
		found := false

		for _, evaluator := range ScopeEvaluators {
			if evaluator.Handles(scope) {
				found = true
				allowedNamespaces, err := evaluator.ResolveGettableNamespaces(scope, clusterPolicyGetter)
				if err != nil {
					errors = append(errors, err)
					continue
				}

				visibleNamespaces.Insert(allowedNamespaces...)
				break
			}
		}

		if !found {
			errors = append(errors, fmt.Errorf("no scope evaluator found for %q", scope))
		}
	}

	return visibleNamespaces, kutilerrors.NewAggregate(errors)
}

const (
	UserIndicator        = "user:"
	ClusterRoleIndicator = "role:"
)

// ScopeEvaluator takes a scope and returns the rules that express it
type ScopeEvaluator interface {
	// Handles returns true if this evaluator can evaluate this scope
	Handles(scope string) bool
	// Validate returns an error if the scope is malformed
	Validate(scope string) error
	// Describe returns a description, warning (typically used to warn about escalation dangers), or an error if the scope is malformed
	Describe(scope string) (description string, warning string, err error)
	// ResolveRules returns the policy rules that this scope allows
	ResolveRules(scope, namespace string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]authorizationapi.PolicyRule, error)
	ResolveGettableNamespaces(scope string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]string, error)
}

// ScopeEvaluators map prefixes to a function that handles that prefix
var ScopeEvaluators = []ScopeEvaluator{
	userEvaluator{},
	clusterRoleEvaluator{},
}

// scopes are in the format
// <indicator><indicator choice>
// we have the following formats:
// user:<scope name>
// role:<clusterrole name>:<namespace to allow the cluster role, * means all>
// TODO
// cluster:<comma-delimited verbs>:<comma-delimited resources>
// namespace:<namespace name>:<comma-delimited verbs>:<comma-delimited resources>

const (
	UserInfo        = UserIndicator + "info"
	UserAccessCheck = UserIndicator + "check-access"

	// UserListScopedProjects gives explicit permission to see the projects that this token can see.
	UserListScopedProjects = UserIndicator + "list-scoped-projects"

	// UserListAllProjects gives explicit permission to see the projects a user can see.  This is often used to prime secondary ACL systems
	// unrelated to openshift and to display projects for selection in a secondary UI.
	UserListAllProjects = UserIndicator + "list-projects"

	// UserFull includes all permissions of the user
	UserFull = UserIndicator + "full"
)

var defaultSupportedScopesMap = map[string]string{
	UserInfo:               "Read-only access to your user information (including username, identities, and group membership)",
	UserAccessCheck:        `Read-only access to view your privileges (for example, "can I create builds?")`,
	UserListScopedProjects: `Read-only access to list your projects viewable with this token and view their metadata (display name, description, etc.)`,
	UserListAllProjects:    `Read-only access to list your projects and view their metadata (display name, description, etc.)`,
	UserFull:               `Full read/write access with all of your permissions`,
}

func DefaultSupportedScopes() []string {
	return sets.StringKeySet(defaultSupportedScopesMap).List()
}

func DefaultSupportedScopesMap() map[string]string {
	return defaultSupportedScopesMap
}

// user:<scope name>
type userEvaluator struct{}

func (userEvaluator) Handles(scope string) bool {
	return strings.HasPrefix(scope, UserIndicator)
}

func (userEvaluator) Validate(scope string) error {
	switch scope {
	case UserFull, UserInfo, UserAccessCheck, UserListScopedProjects, UserListAllProjects:
		return nil
	}

	return fmt.Errorf("unrecognized scope: %v", scope)
}

func (userEvaluator) Describe(scope string) (string, string, error) {
	switch scope {
	case UserInfo, UserAccessCheck, UserListScopedProjects, UserListAllProjects:
		return defaultSupportedScopesMap[scope], "", nil
	case UserFull:
		return defaultSupportedScopesMap[scope], `Includes any access you have to escalating resources like secrets`, nil
	default:
		return "", "", fmt.Errorf("unrecognized scope: %v", scope)
	}
}

func (userEvaluator) ResolveRules(scope, namespace string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]authorizationapi.PolicyRule, error) {
	switch scope {
	case UserInfo:
		return []authorizationapi.PolicyRule{
			{Verbs: sets.NewString("get"), APIGroups: []string{userapi.GroupName, userapi.LegacyGroupName}, Resources: sets.NewString("users"), ResourceNames: sets.NewString("~")},
		}, nil
	case UserAccessCheck:
		return []authorizationapi.PolicyRule{
			authorizationapi.NewRule("create").Groups(kauthorizationapi.GroupName).Resources("selfsubjectaccessreviews").RuleOrDie(),
			authorizationapi.NewRule("create").Groups(authorizationapi.GroupName, authorizationapi.LegacyGroupName).Resources("selfsubjectrulesreviews").RuleOrDie(),
		}, nil
	case UserListScopedProjects:
		return []authorizationapi.PolicyRule{
			{Verbs: sets.NewString("list", "watch"), APIGroups: []string{projectapi.GroupName, projectapi.LegacyGroupName}, Resources: sets.NewString("projects")},
		}, nil
	case UserListAllProjects:
		return []authorizationapi.PolicyRule{
			{Verbs: sets.NewString("list", "watch"), APIGroups: []string{projectapi.GroupName, projectapi.LegacyGroupName}, Resources: sets.NewString("projects")},
			{Verbs: sets.NewString("get"), APIGroups: []string{kapi.GroupName}, Resources: sets.NewString("namespaces")},
		}, nil
	case UserFull:
		return []authorizationapi.PolicyRule{
			{Verbs: sets.NewString("*"), APIGroups: []string{"*"}, Resources: sets.NewString("*")},
			{Verbs: sets.NewString("*"), NonResourceURLs: sets.NewString("*")},
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized scope: %v", scope)
	}
}

func (userEvaluator) ResolveGettableNamespaces(scope string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]string, error) {
	switch scope {
	case UserFull, UserListAllProjects:
		return []string{"*"}, nil
	default:
		return []string{}, nil
	}
}

// escalatingScopeResources are resources that are considered escalating for scope evaluation
var escalatingScopeResources = []schema.GroupResource{
	{Group: kapi.GroupName, Resource: "secrets"},

	{Group: imageapi.GroupName, Resource: "imagestreams/secrets"},
	{Group: imageapi.LegacyGroupName, Resource: "imagestreams/secrets"},

	{Group: oauthapi.GroupName, Resource: "oauthauthorizetokens"},
	{Group: oauthapi.LegacyGroupName, Resource: "oauthauthorizetokens"},

	{Group: oauthapi.GroupName, Resource: "oauthaccesstokens"},
	{Group: oauthapi.LegacyGroupName, Resource: "oauthaccesstokens"},

	{Group: authorizationapi.GroupName, Resource: "roles"},
	{Group: authorizationapi.LegacyGroupName, Resource: "roles"},

	{Group: authorizationapi.GroupName, Resource: "rolebindings"},
	{Group: authorizationapi.LegacyGroupName, Resource: "rolebindings"},

	{Group: authorizationapi.GroupName, Resource: "clusterroles"},
	{Group: authorizationapi.LegacyGroupName, Resource: "clusterroles"},

	{Group: authorizationapi.GroupName, Resource: "clusterrolebindings"},
	{Group: authorizationapi.LegacyGroupName, Resource: "clusterrolebindings"},
}

// role:<clusterrole name>:<namespace to allow the cluster role, * means all>
type clusterRoleEvaluator struct{}

var clusterRoleEvaluatorInstance = clusterRoleEvaluator{}

func (clusterRoleEvaluator) Handles(scope string) bool {
	return strings.HasPrefix(scope, ClusterRoleIndicator)
}

func (e clusterRoleEvaluator) Validate(scope string) error {
	_, _, _, err := e.parseScope(scope)
	return err
}

// parseScope parses the requested scope, determining the requested role name, namespace, and if
// access to escalating objects is required.  It will return an error if it doesn't parse cleanly
func (e clusterRoleEvaluator) parseScope(scope string) (string /*role name*/, string /*namespace*/, bool /*escalating*/, error) {
	if !e.Handles(scope) {
		return "", "", false, fmt.Errorf("bad format for scope %v", scope)
	}
	return ParseClusterRoleScope(scope)
}
func ParseClusterRoleScope(scope string) (string /*role name*/, string /*namespace*/, bool /*escalating*/, error) {
	if !strings.HasPrefix(scope, ClusterRoleIndicator) {
		return "", "", false, fmt.Errorf("bad format for scope %v", scope)
	}
	escalating := false
	if strings.HasSuffix(scope, ":!") {
		escalating = true
		// clip that last segment before parsing the rest
		scope = scope[:strings.LastIndex(scope, ":")]
	}

	tokens := strings.SplitN(scope, ":", 2)
	if len(tokens) != 2 {
		return "", "", false, fmt.Errorf("bad format for scope %v", scope)
	}

	// namespaces can't have colons, but roles can.  pick last.
	lastColonIndex := strings.LastIndex(tokens[1], ":")
	if lastColonIndex <= 0 || lastColonIndex == (len(tokens[1])-1) {
		return "", "", false, fmt.Errorf("bad format for scope %v", scope)
	}

	return tokens[1][0:lastColonIndex], tokens[1][lastColonIndex+1:], escalating, nil
}

func (e clusterRoleEvaluator) Describe(scope string) (string, string, error) {
	roleName, scopeNamespace, escalating, err := e.parseScope(scope)
	if err != nil {
		return "", "", err
	}

	// Anything you can do [in project "foo" | server-wide] that is also allowed by the "admin" role[, except access escalating resources like secrets]

	scopePhrase := ""
	if scopeNamespace == authorizationapi.ScopesAllNamespaces {
		scopePhrase = "server-wide"
	} else {
		scopePhrase = fmt.Sprintf("in project %q", scopeNamespace)
	}

	warning := ""
	escalatingPhrase := ""
	if escalating {
		warning = fmt.Sprintf("Includes access to escalating resources like secrets")
	} else {
		escalatingPhrase = ", except access escalating resources like secrets"
	}

	description := fmt.Sprintf("Anything you can do %s that is also allowed by the %q role%s", scopePhrase, roleName, escalatingPhrase)

	return description, warning, nil
}

func (e clusterRoleEvaluator) ResolveRules(scope, namespace string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]authorizationapi.PolicyRule, error) {
	_, scopeNamespace, _, err := e.parseScope(scope)
	if err != nil {
		return nil, err
	}

	// if the scope limit on the clusterrole doesn't match, then don't add any rules, but its not an error
	if !(scopeNamespace == authorizationapi.ScopesAllNamespaces || scopeNamespace == namespace) {
		return []authorizationapi.PolicyRule{}, nil
	}

	return e.resolveRules(scope, clusterPolicyGetter)
}

// resolveRules doesn't enforce namespace checks
func (e clusterRoleEvaluator) resolveRules(scope string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]authorizationapi.PolicyRule, error) {
	roleName, _, escalating, err := e.parseScope(scope)
	if err != nil {
		return nil, err
	}

	policy, err := clusterPolicyGetter.Get("default")
	if err != nil {
		return nil, err
	}
	role, exists := policy.Roles[roleName]
	if !exists {
		return nil, kapierrors.NewNotFound(authorizationapi.LegacyResource("clusterrole"), roleName)
	}

	rules := []authorizationapi.PolicyRule{}
	for _, rule := range role.Rules {
		if escalating {
			rules = append(rules, rule)
			continue
		}

		// rules with unbounded access shouldn't be allowed in scopes.
		if rule.Verbs.Has(authorizationapi.VerbAll) || rule.Resources.Has(authorizationapi.ResourceAll) || getAPIGroupSet(rule).Has(authorizationapi.APIGroupAll) {
			continue
		}
		// rules that allow escalating resource access should be cleaned.
		safeRule := removeEscalatingResources(rule)
		rules = append(rules, safeRule)
	}

	return rules, nil
}

func (e clusterRoleEvaluator) ResolveGettableNamespaces(scope string, clusterPolicyGetter authorizationlister.ClusterPolicyLister) ([]string, error) {
	_, scopeNamespace, _, err := e.parseScope(scope)
	if err != nil {
		return nil, err
	}
	rules, err := e.resolveRules(scope, clusterPolicyGetter)
	if err != nil {
		return nil, err
	}

	attributes := kauthorizer.AttributesRecord{
		APIGroup:        kapi.GroupName,
		Verb:            "get",
		Resource:        "namespaces",
		ResourceRequest: true,
	}

	errors := []error{}
	for _, rule := range rules {
		matches, err := authorizer.RuleMatches(attributes, rule)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		if matches {
			return []string{scopeNamespace}, nil
		}
	}

	return []string{}, kutilerrors.NewAggregate(errors)
}

// TODO: direct deep copy needing a cloner is something that should be fixed upstream
var localCloner = conversion.NewCloner()

// removeEscalatingResources inspects a PolicyRule and removes any references to escalating resources.
// It has coarse logic for now.  It is possible to rewrite one rule into many for the finest grain control
// but removing the entire matching resource regardless of verb or secondary group is cheaper, easier, and errs on the side removing
// too much, not too little
func removeEscalatingResources(in authorizationapi.PolicyRule) authorizationapi.PolicyRule {
	var ruleCopy *authorizationapi.PolicyRule

	apiGroups := getAPIGroupSet(in)
	for _, resource := range escalatingScopeResources {
		if !(apiGroups.Has(resource.Group) && in.Resources.Has(resource.Resource)) {
			continue
		}

		if ruleCopy == nil {
			// we're using a cache of cache of an object that uses pointers to data.  I'm pretty sure we need to do a copy to avoid
			// muddying the cache
			ruleCopy = &authorizationapi.PolicyRule{}
			authorizationapi.DeepCopy_authorization_PolicyRule(&in, ruleCopy, localCloner)
		}

		ruleCopy.Resources.Delete(resource.Resource)
	}

	if ruleCopy != nil {
		return *ruleCopy
	}

	return in
}

func getAPIGroupSet(rule authorizationapi.PolicyRule) sets.String {
	apiGroups := sets.NewString(rule.APIGroups...)
	if len(apiGroups) == 0 {
		// this was done for backwards compatibility in the authorizer
		apiGroups.Insert("")
	}

	return apiGroups
}

func ValidateScopeRestrictions(client *oauthapi.OAuthClient, scopes ...string) error {
	if len(client.ScopeRestrictions) == 0 {
		return nil
	}
	if len(scopes) == 0 {
		return fmt.Errorf("%v may not request unscoped tokens", client.Name)
	}

	errs := []error{}
	for _, scope := range scopes {
		if err := validateScopeRestrictions(client, scope); err != nil {
			errs = append(errs, err)
		}
	}

	return kutilerrors.NewAggregate(errs)
}

func validateScopeRestrictions(client *oauthapi.OAuthClient, scope string) error {
	errs := []error{}

	if len(client.ScopeRestrictions) == 0 {
		return nil
	}

	for _, restriction := range client.ScopeRestrictions {
		if len(restriction.ExactValues) > 0 {
			if err := ValidateLiteralScopeRestrictions(scope, restriction.ExactValues); err != nil {
				errs = append(errs, err)
				continue
			}
			return nil
		}

		if restriction.ClusterRole != nil {
			if !clusterRoleEvaluatorInstance.Handles(scope) {
				continue
			}
			if err := ValidateClusterRoleScopeRestrictions(scope, *restriction.ClusterRole); err != nil {
				errs = append(errs, err)
				continue
			}
			return nil
		}
	}

	// if we got here, then nothing matched.   If we already have errors, do nothing, otherwise add one to make it report failed.
	if len(errs) == 0 {
		errs = append(errs, fmt.Errorf("%v did not match any scope restriction", scope))
	}

	return kutilerrors.NewAggregate(errs)
}

func ValidateLiteralScopeRestrictions(scope string, literals []string) error {
	for _, literal := range literals {
		if literal == scope {
			return nil
		}
	}

	return fmt.Errorf("%v not found in %v", scope, literals)
}

func ValidateClusterRoleScopeRestrictions(scope string, restriction oauthapi.ClusterRoleScopeRestriction) error {
	role, namespace, escalating, err := clusterRoleEvaluatorInstance.parseScope(scope)
	if err != nil {
		return err
	}

	foundName := false
	for _, restrictedRoleName := range restriction.RoleNames {
		if restrictedRoleName == "*" || restrictedRoleName == role {
			foundName = true
			break
		}
	}
	if !foundName {
		return fmt.Errorf("%v does not use an approved name", scope)
	}

	foundNamespace := false
	for _, restrictedNamespace := range restriction.Namespaces {
		if restrictedNamespace == "*" || restrictedNamespace == namespace {
			foundNamespace = true
			break
		}
	}
	if !foundNamespace {
		return fmt.Errorf("%v does not use an approved namespace", scope)
	}

	if escalating && !restriction.AllowEscalation {
		return fmt.Errorf("%v is not allowed to escalate", scope)
	}

	return nil
}
