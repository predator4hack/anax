package compcheck

import (
	"fmt"
	"github.com/open-horizon/anax/businesspolicy"
	"github.com/open-horizon/anax/common"
	"github.com/open-horizon/anax/cutil"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/i18n"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/semanticversion"
	"golang.org/x/text/message"
	"strings"
)

// The input format for the userinput check
type UserInputCheck struct {
	NodeId         string                         `json:"node_id,omitempty"`
	NodeArch       string                         `json:"node_arch,omitempty"`
	NodeUserInput  []policy.UserInput             `json:"node_user_input,omitempty"`
	BusinessPolId  string                         `json:"business_policy_id,omitempty"`
	BusinessPolicy *businesspolicy.BusinessPolicy `json:"business_policy,omitempty"`
	PatternId      string                         `json:"pattern_id,omitempty"`
	Pattern        *common.PatternFile            `json:"pattern,omitempty"`
	Service        []common.ServiceFile           `json:"service,omitempty"`
	ServiceToCheck []string                       `json:"service_to_check,omitempty"` // for internal use for performance. only check the service with the ids. If empty, check all.
}

func (p UserInputCheck) String() string {
	return fmt.Sprintf("NodeId: %v, NodeArch: %v, NodeUserInput: %v, BusinessPolId: %v, BusinessPolicy: %v, PatternId: %v, Pattern: %v, Service: %v,",
		p.NodeId, p.NodeArch, p.NodeUserInput, p.BusinessPolId, p.BusinessPolicy, p.PatternId, p.Pattern, p.Service)
}

type ServiceDefinition struct {
	Org string `json:"org"`
	exchange.ServiceDefinition
}

func (s *ServiceDefinition) GetOrg() string {
	return s.Org
}

func (s *ServiceDefinition) GetURL() string {
	return s.URL
}

func (s *ServiceDefinition) GetVersion() string {
	return s.Version
}

func (s *ServiceDefinition) GetArch() string {
	return s.Arch
}

func (s *ServiceDefinition) GetRequiredServices() []exchange.ServiceDependency {
	return s.RequiredServices
}

func (s *ServiceDefinition) GetUserInputs() []exchange.UserInput {
	return s.UserInputs
}

func (s *ServiceDefinition) NeedsUserInput() bool {
	if s.UserInputs == nil || len(s.UserInputs) == 0 {
		return false
	}

	for _, ui := range s.UserInputs {
		if ui.Name != "" && ui.DefaultValue == "" {
			return true
		}
	}
	return false
}

type Pattern struct {
	Org string `json:"org"`
	exchange.Pattern
}

func (p *Pattern) GetOrg() string {
	return p.Org
}

func (p *Pattern) GetServices() []exchange.ServiceReference {
	return p.Services
}

func (p *Pattern) GetUserInputs() []policy.UserInput {
	return p.UserInput
}

type ServiceSpec struct {
	ServiceOrgid        string `json:"serviceOrgid"`
	ServiceUrl          string `json:"serviceUrl"`
	ServiceArch         string `json:"serviceArch"`
	ServiceVersionRange string `json:"serviceVersionRange"` // version or version range. empty string means it applies to all versions
}

func (s ServiceSpec) String() string {
	return fmt.Sprintf("ServiceOrgid: %v, "+
		"ServiceUrl: %v, "+
		"ServiceArch: %v, "+
		"ServiceVersionRange: %v",
		s.ServiceOrgid, s.ServiceUrl, s.ServiceArch, s.ServiceVersionRange)
}

func NewServiceSpec(svcName, svcOrg, svcVersion, svcArch string) *ServiceSpec {
	return &ServiceSpec{
		ServiceOrgid:        svcOrg,
		ServiceUrl:          svcName,
		ServiceArch:         svcArch,
		ServiceVersionRange: svcVersion,
	}
}

// This is the function that HZN and the agbot secure API calls.
// Given the UserInoutCheck input, check if the user inputs are compatible.
// The required fields in UserInputCheck are:
//  (NodeId or NodeUserInput) and (BusinessPolId or BusinessPolicy)
//
// When checking whether the user inputs are compatible or not, we need to merge the node's user input
// with the ones in the business policy and check them against the user input requirements in the top level
// and dependent services.
func UserInputCompatible(ec exchange.ExchangeContext, uiInput *UserInputCheck, checkAllSvcs bool, msgPrinter *message.Printer) (*CompCheckOutput, error) {

	getDeviceHandler := exchange.GetHTTPDeviceHandler(ec)
	getBusinessPolicies := exchange.GetHTTPBusinessPoliciesHandler(ec)
	getPatterns := exchange.GetHTTPExchangePatternHandler(ec)
	getServiceHandler := exchange.GetHTTPServiceHandler(ec)
	serviceDefResolverHandler := exchange.GetHTTPServiceDefResolverHandler(ec)
	getSelectedServices := exchange.GetHTTPSelectedServicesHandler(ec)

	return userInputCompatible(getDeviceHandler, getBusinessPolicies, getPatterns, getServiceHandler, serviceDefResolverHandler, getSelectedServices, uiInput, checkAllSvcs, msgPrinter)
}

// Internal function for UserInputCompatible
func userInputCompatible(getDeviceHandler exchange.DeviceHandler,
	getBusinessPolicies exchange.BusinessPoliciesHandler,
	getPatterns exchange.PatternHandler,
	getServiceHandler exchange.ServiceHandler,
	serviceDefResolverHandler exchange.ServiceDefResolverHandler,
	getSelectedServices exchange.SelectedServicesHandler,
	uiInput *UserInputCheck, checkAllSvcs bool, msgPrinter *message.Printer) (*CompCheckOutput, error) {

	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	if uiInput == nil {
		return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("The UserInputCheck input cannot be null")), COMPCHECK_INPUT_ERROR)
	}

	// make a copy of the input because the process will change it. The pointer to policies will stay the same.
	input_temp := UserInputCheck(*uiInput)
	input := &input_temp

	resources := NewCompCheckResourceFromUICheck(uiInput)

	// get user input from node if node id is specified.
	var nodeUserInput []policy.UserInput
	nodeId := input.NodeId
	if input.NodeUserInput != nil {
		nodeUserInput = input.NodeUserInput
	} else if nodeId != "" {
		node, err := GetExchangeNode(getDeviceHandler, nodeId, msgPrinter)
		if err != nil {
			return nil, err
		} else if input.NodeArch != "" {
			if node.Arch != "" && node.Arch != input.NodeArch {
				return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("The input node architecture %v does not match the exchange node architecture %v for node %v.", input.NodeArch, node.Arch, nodeId)), COMPCHECK_INPUT_ERROR)
			}
		} else {
			resources.NodeArch = node.Arch
		}

		resources.NodeUserInput = node.UserInput
		nodeUserInput = node.UserInput
	} else {
		return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Neither node user input nor node id is specified.")), COMPCHECK_INPUT_ERROR)
	}

	// make sure only specify one: business policy or pattern
	useBPol := false
	if input.BusinessPolId != "" || input.BusinessPolicy != nil {
		useBPol = true
		if input.PatternId != "" || input.Pattern != nil {
			return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Bussiness policy and pattern are mutually exclusive.")), COMPCHECK_INPUT_ERROR)
		}
	} else {
		if input.PatternId == "" && input.Pattern == nil {
			return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Neither bussiness policy nor pattern is specified.")), COMPCHECK_INPUT_ERROR)
		}
	}

	// validate the business policy/pattern, get the user input and workloads from them.
	var bpUserInput []policy.UserInput
	var serviceRefs []exchange.ServiceReference
	if useBPol {
		bPolicy, _, err := processBusinessPolicy(getBusinessPolicies, input.BusinessPolId, input.BusinessPolicy, false, msgPrinter)
		if err != nil {
			return nil, err
		} else if input.BusinessPolicy == nil {
			resources.BusinessPolicy = bPolicy
		}
		bpUserInput = bPolicy.UserInput
		serviceRefs = getWorkloadsFromBPol(bPolicy, resources.NodeArch)
	} else {
		pattern, err := processPattern(getPatterns, input.PatternId, input.Pattern, msgPrinter)
		if err != nil {
			return nil, err
		} else if input.Pattern == nil {
			resources.Pattern = pattern
		}
		bpUserInput = pattern.GetUserInputs()
		serviceRefs = getWorkloadsFromPattern(pattern, resources.NodeArch)
	}
	if serviceRefs == nil || len(serviceRefs) == 0 {
		if resources.NodeArch != "" {
			return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("No service versions with architecture %v specified in the business policy or pattern.", resources.NodeArch)), COMPCHECK_VALIDATION_ERROR)
		} else {
			return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("No service versions specified in the business policy or pattern.")), COMPCHECK_VALIDATION_ERROR)
		}
	}

	// check if the given services match the services defined in the business policy or pattern
	if err := validateServices(resources.Service, resources.BusinessPolicy, resources.Pattern, input.ServiceToCheck, msgPrinter); err != nil {
		return nil, err
	}
	inServices := input.Service

	messages := map[string]string{}
	msg_incompatible := msgPrinter.Sprintf("User Input Incompatible")
	msg_compatible := msgPrinter.Sprintf("Compatible")

	// go through all the workloads and check if user input is compatible or not
	service_comp := []common.AbstractServiceFile{}
	service_incomp := []common.AbstractServiceFile{}
	overall_compatible := true
	for _, serviceRef := range serviceRefs {
		service_compatible := false
		for _, workload := range serviceRef.ServiceVersions {

			// get service + dependen services and then compare the user inputs
			if inServices == nil || len(inServices) == 0 {
				if serviceRef.ServiceArch != "*" && serviceRef.ServiceArch != "" {
					sId := cutil.FormExchangeIdForService(serviceRef.ServiceURL, workload.Version, serviceRef.ServiceArch)
					sId = fmt.Sprintf("%v/%v", serviceRef.ServiceOrg, sId)
					if !needHandleService(sId, input.ServiceToCheck) {
						continue
					}
					sSpec := NewServiceSpec(serviceRef.ServiceURL, serviceRef.ServiceOrg, workload.Version, serviceRef.ServiceArch)
					if compatible, reason, sDef, err := VerifyUserInputForService(sSpec, getServiceHandler, serviceDefResolverHandler, bpUserInput, nodeUserInput, msgPrinter); err != nil {
						return nil, err
					} else {
						if compatible {
							service_compatible = true
							service_comp = append(service_comp, sDef)
							messages[sId] = msg_compatible
							if !checkAllSvcs {
								break
							}
						} else {
							service_incomp = append(service_incomp, sDef)
							messages[sId] = fmt.Sprintf("%v: %v", msg_incompatible, reason)
						}
					}
				} else {
					// since workload arch is empty, need to go through all the arches
					if svcMeta, err := getSelectedServices(serviceRef.ServiceURL, serviceRef.ServiceOrg, workload.Version, ""); err != nil {
						return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error getting services for all archetctures for %v/%v version %v. %v", serviceRef.ServiceOrg, serviceRef.ServiceURL, workload.Version, err)), COMPCHECK_EXCHANGE_ERROR)
					} else {
						for sId, s := range svcMeta {
							org := exchange.GetOrg(sId)
							svc := ServiceDefinition{org, s}
							if !needHandleService(sId, input.ServiceToCheck) {
								continue
							}
							if compatible, reason, _, err := VerifyUserInputForServiceDef(&svc, getServiceHandler, serviceDefResolverHandler, bpUserInput, nodeUserInput, msgPrinter); err != nil {
								return nil, err
							} else {
								if compatible {
									service_compatible = true
									service_comp = append(service_comp, &svc)
									messages[sId] = msg_compatible
									if !checkAllSvcs {
										break
									}
								} else {
									service_incomp = append(service_incomp, &svc)
									messages[sId] = fmt.Sprintf("%v: %v", msg_incompatible, reason)
								}
							}
						}
						if service_compatible && !checkAllSvcs {
							break
						}
					}
				}
			} else {
				found := false
				var useSDef common.AbstractServiceFile
				for _, in_svc := range inServices {
					if in_svc.GetURL() == serviceRef.ServiceURL && in_svc.GetVersion() == workload.Version &&
						(serviceRef.ServiceArch == "*" || serviceRef.ServiceArch == "" || in_svc.GetArch() == serviceRef.ServiceArch) &&
						(in_svc.GetOrg() == "" || in_svc.GetOrg() == serviceRef.ServiceOrg) {
						found = true
						useSDef = &in_svc
						break
					}
				}

				sId := cutil.FormExchangeIdForService(serviceRef.ServiceURL, workload.Version, serviceRef.ServiceArch)
				sId = fmt.Sprintf("%v/%v", serviceRef.ServiceOrg, sId)
				if !needHandleService(sId, input.ServiceToCheck) {
					continue
				}
				if !found {
					messages[sId] = fmt.Sprintf("%v: %v", msg_incompatible, msgPrinter.Sprintf("Service definition not found in the input."))
				} else {
					if useSDef.GetOrg() == "" {
						useSDef.(*common.ServiceFile).Org = serviceRef.ServiceOrg
					}
					if compatible, reason, sDef, err := VerifyUserInputForServiceDef(useSDef, getServiceHandler, serviceDefResolverHandler, bpUserInput, nodeUserInput, msgPrinter); err != nil {
						return nil, err
					} else {
						if compatible {
							service_compatible = true
							service_comp = append(service_comp, sDef)
							messages[sId] = msg_compatible
							if !checkAllSvcs {
								break
							}
						} else {
							service_incomp = append(service_incomp, sDef)
							messages[sId] = fmt.Sprintf("%v: %v", msg_incompatible, reason)
						}
					}
				}
			}
		}

		// overall_compatible turn to false if any service is not compatible
		if !service_compatible {
			overall_compatible = false
		}
	}

	// If we get here, it means that no workload is found in the bp/pattern that matches the required node arch.
	if messages != nil && len(messages) != 0 {
		if overall_compatible {
			resources.Service = service_comp
		} else {
			resources.Service = service_incomp
		}
		return NewCompCheckOutput(overall_compatible, messages, resources), nil
	} else {
		if resources.NodeArch != "" {
			messages["general"] = fmt.Sprintf("%v: %v", msg_incompatible, msgPrinter.Sprintf("Service with 'arch' %v cannot be found in the business policy or pattern.", resources.NodeArch))
		} else {
			messages["general"] = fmt.Sprintf("%v: %v", msg_incompatible, msgPrinter.Sprintf("No services found in the business policy or pattern."))
		}

		return NewCompCheckOutput(false, messages, resources), nil
	}
}

// This function does the following:
// 1. go to the exchange and gets the service and its dependent services.
// 2. merge the user input from business policy and node.
// 3. check if the merged user input satisfies the service requirements.
func VerifyUserInputForService(svcSpec *ServiceSpec,
	getServiceHandler exchange.ServiceHandler,
	serviceDefResolverHandler exchange.ServiceDefResolverHandler,
	bpUserInput []policy.UserInput,
	deviceUserInput []policy.UserInput,
	msgPrinter *message.Printer) (bool, string, *ServiceDefinition, error) {

	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	// nothing to check
	if svcSpec == nil {
		return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("The input service spec object cannot be null.")), COMPCHECK_INPUT_ERROR)
	}

	svc_map, sDef, sId, err := serviceDefResolverHandler(svcSpec.ServiceUrl, svcSpec.ServiceOrgid, svcSpec.ServiceVersionRange, svcSpec.ServiceArch)
	if err != nil {
		return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error retrieving service from the exchange for %v. %v", svcSpec, err)), COMPCHECK_EXCHANGE_ERROR)
	}

	compSDef := ServiceDefinition{svcSpec.ServiceOrgid, *sDef}

	if compatible, reason, _, err := VerifyUserInputForSingleServiceDef(&compSDef, bpUserInput, deviceUserInput, msgPrinter); err != nil {
		return false, "", &compSDef, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error verifing user input for service %v. %v", sId, err)), COMPCHECK_GENERAL_ERROR)
	} else if !compatible {
		return false, msgPrinter.Sprintf("Failed to verify user input for service %v. %v", sId, reason), &compSDef, nil
	} else {
		for id, s := range svc_map {
			org := exchange.GetOrg(id)
			svc := ServiceDefinition{org, s}
			if compatible, reason, _, err := VerifyUserInputForSingleServiceDef(&svc, bpUserInput, deviceUserInput, msgPrinter); err != nil {
				return false, "", &compSDef, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error verifing user input for dependent service %v. %v", id, err)), COMPCHECK_GENERAL_ERROR)
			} else if !compatible {
				return false, msgPrinter.Sprintf("Failed to verify user input for dependent service %v. %v", id, reason), &compSDef, nil
			}
		}
	}

	return true, "", &compSDef, nil
}

// This function does the following:
// 1. go to the exchange and gets the dependent services if any
// 2. merge the user input from business policy and node.
// 3. check if the merged user input satisfies the service requirements.
func VerifyUserInputForServiceDef(sDef common.AbstractServiceFile,
	getServiceHandler exchange.ServiceHandler,
	serviceDefResolverHandler exchange.ServiceDefResolverHandler,
	bpUserInput []policy.UserInput,
	deviceUserInput []policy.UserInput,
	msgPrinter *message.Printer) (bool, string, common.AbstractServiceFile, error) {

	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	// nothing to check
	if sDef == nil {
		return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("The input service definition object cannot be null.")), COMPCHECK_INPUT_ERROR)
	}

	// verify top level services
	sId := cutil.FormExchangeIdForService(sDef.GetURL(), sDef.GetVersion(), sDef.GetArch())
	sId = fmt.Sprintf("%v/%v", sDef.GetOrg(), sId)
	if compatible, reason, _, err := VerifyUserInputForSingleServiceDef(sDef, bpUserInput, deviceUserInput, msgPrinter); err != nil {
		return false, "", sDef, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error verifing user input for service %v. %v", sId, err)), COMPCHECK_GENERAL_ERROR)
	} else if !compatible {
		return false, msgPrinter.Sprintf("Failed to verify user input for service %v. %v", sId, reason), sDef, nil
	}

	// get all the service defs for the dependent services
	service_map := map[string]ServiceDefinition{}
	if sDef.GetRequiredServices() != nil && len(sDef.GetRequiredServices()) != 0 {
		for _, sDep := range sDef.GetRequiredServices() {
			if vExp, err := semanticversion.Version_Expression_Factory(sDep.Version); err != nil {
				return false, "", sDef, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Unable to create version expression from %v. %v", sDep.Version, err)), COMPCHECK_GENERAL_ERROR)
			} else {
				if s_map, s_def, s_id, err := serviceDefResolverHandler(sDep.URL, sDep.Org, vExp.Get_expression(), sDep.Arch); err != nil {
					return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error retrieving dependent services from the exchange for %v. %v", sDep, err)), COMPCHECK_EXCHANGE_ERROR)
				} else {
					service_map[s_id] = ServiceDefinition{sDep.Org, *s_def}
					for id, s := range s_map {
						service_map[id] = ServiceDefinition{exchange.GetOrg(id), s}
					}
				}
			}
		}
	}

	// verify dependent services
	for id, s := range service_map {
		if compatible, reason, _, err := VerifyUserInputForSingleServiceDef(&s, bpUserInput, deviceUserInput, msgPrinter); err != nil {
			return false, "", sDef, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error verifing user input for dependent service %v. %v", id, err)), COMPCHECK_GENERAL_ERROR)
		} else if !compatible {
			return false, msgPrinter.Sprintf("Failed to verify user input for dependent service %v. %v", id, reason), sDef, nil
		}
	}

	return true, "", sDef, nil
}

// Verfiy that all userInput variables are correctly typed and that non-defaulted userInput variables are specified.
func VerifyUserInputForSingleService(svcSpec *ServiceSpec,
	getService exchange.ServiceHandler,
	bpUserInput []policy.UserInput,
	deviceUserInput []policy.UserInput,
	msgPrinter *message.Printer) (bool, string, common.AbstractServiceFile, error) {

	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	// get the service from the exchange
	vExp, _ := semanticversion.Version_Expression_Factory(svcSpec.ServiceVersionRange)
	sdef, sId, err := getService(svcSpec.ServiceUrl, svcSpec.ServiceOrgid, vExp.Get_expression(), svcSpec.ServiceArch)
	if err != nil {
		return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Failed to get the service from the exchange. %v", err)), COMPCHECK_EXCHANGE_ERROR)
	} else if sdef == nil {
		return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Servcie does not exist on the exchange.")), COMPCHECK_EXCHANGE_ERROR)
	}

	svc := ServiceDefinition{exchange.GetOrg(sId), *sdef}
	return VerifyUserInputForSingleServiceDef(&svc, bpUserInput, deviceUserInput, msgPrinter)
}

// Verfiy that all userInput variables are correctly typed and that non-defaulted userInput variables are specified.
func VerifyUserInputForSingleServiceDef(sdef common.AbstractServiceFile,
	bpUserInput []policy.UserInput, deviceUserInput []policy.UserInput, msgPrinter *message.Printer) (bool, string, common.AbstractServiceFile, error) {

	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	// nothing to check
	if sdef == nil {
		return false, "", nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("The input service definition object cannot be null.")), COMPCHECK_INPUT_ERROR)
	}

	// service does not need user input
	if !sdef.NeedsUserInput() {
		return true, "", sdef, nil
	}

	// service needs user input, find the correct elements in the array
	var mergedUI *policy.UserInput
	ui1, err := policy.FindUserInput(sdef.GetURL(), sdef.GetOrg(), sdef.GetVersion(), sdef.GetArch(), bpUserInput)
	if err != nil {
		return false, "", sdef, NewCompCheckError(err, COMPCHECK_GENERAL_ERROR)
	}
	ui2, err := policy.FindUserInput(sdef.GetURL(), sdef.GetOrg(), sdef.GetVersion(), sdef.GetArch(), deviceUserInput)
	if err != nil {
		return false, "", sdef, NewCompCheckError(err, COMPCHECK_GENERAL_ERROR)
	}

	if ui1 == nil && ui2 == nil {
		return false, msgPrinter.Sprintf("No user input found for service."), sdef, nil
	}

	if ui1 != nil && ui2 != nil {
		mergedUI, _ = policy.MergeUserInput(*ui1, *ui2, false)
	} else if ui1 != nil {
		mergedUI = ui1
	} else {
		mergedUI = ui2
	}

	// Verify that non-default variables are present.
	for _, ui := range sdef.GetUserInputs() {
		found := false
		for _, mui := range mergedUI.Inputs {
			if ui.Name == mui.Name {
				found = true
				if err := cutil.VerifyWorkloadVarTypes(mui.Value, ui.Type); err != nil {
					return false, msgPrinter.Sprintf("Failed to validate the user input type for variable %v. %v", ui.Name, err), sdef, nil
				}
				break
			}
		}

		if !found && ui.DefaultValue == "" {
			return false, msgPrinter.Sprintf("A required user input value is missing for variable %v.", ui.Name), sdef, nil
		}
	}

	return true, "", sdef, nil
}

// This function makes sure that the given service matches the service specified in the business policy
func validateServiceWithBPolicy(service common.AbstractServiceFile, bPolicy *businesspolicy.BusinessPolicy, msgPrinter *message.Printer) error {

	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	// make sure url is same
	if service.GetURL() != bPolicy.Service.Name {
		return fmt.Errorf(msgPrinter.Sprintf("Service URL %v does not match the service URL %v specified in the business policy.", service.GetURL(), bPolicy.Service.Name))
	}

	if service.GetOrg() != bPolicy.Service.Org {
		return fmt.Errorf(msgPrinter.Sprintf("Service Org %v does not match the service org %v specified in the business policy.", service.GetOrg(), bPolicy.Service.Org))
	}

	// make sure arch is same
	if bPolicy.Service.Arch != "" && bPolicy.Service.Arch != "*" {
		if service.GetArch() != bPolicy.Service.Arch {
			return fmt.Errorf(msgPrinter.Sprintf("Service architecure %v does not match the service architectrure %v specified in the business policy.", service.GetArch(), bPolicy.Service.Arch))
		}
	}

	// make sure version is same
	if bPolicy.Service.ServiceVersions != nil {
		found := false
		for _, v := range bPolicy.Service.ServiceVersions {
			if v.Version == service.GetVersion() {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf(msgPrinter.Sprintf("Service version %v does not match any service versions specified in the business policy.", service.GetVersion()))
		}
	}
	return nil
}

// This function makes sure that the given service matches the service specified in the pattern
func validateServiceWithPattern(service common.AbstractServiceFile, pattern common.AbstractPatternFile, msgPrinter *message.Printer) error {
	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	if pattern.GetServices() == nil {
		return nil
	}

	found := false
	for _, sref := range pattern.GetServices() {
		if service.GetURL() == sref.ServiceURL && service.GetOrg() == sref.ServiceOrg && (sref.ServiceArch == "" || sref.ServiceArch == "*" || service.GetArch() == sref.ServiceArch) {
			for _, v := range sref.ServiceVersions {
				if service.GetVersion() == v.Version {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}

	if found {
		return nil
	} else {
		return fmt.Errorf(msgPrinter.Sprintf("The service does not match any services in the pattern."))
	}
}

// This function checks if the given service id will be processed. The second argument
// contains the service id's that will be process. If it is empty, it means all services
// will be processed.
func needHandleService(sId string, services []string) bool {
	if services == nil || len(services) == 0 {
		return true
	}

	for _, id := range services {
		if strings.HasSuffix(id, "_*") || strings.HasSuffix(id, "_") {
			// if the id ends with _*, it means that the id apply to any arch
			// only compare the part without arch
			id_no_arch := cutil.RemoveArchFromServiceId(id)
			sId_no_arch := cutil.RemoveArchFromServiceId(sId)
			if id_no_arch == sId_no_arch {
				return true
			}
		} else if id == sId {
			return true
		}
	}

	return false
}

// If the inputPat is given, then validate it.
// If not, get patern from the exchange.
func processPattern(getPatterns exchange.PatternHandler, patId string, inputPat *common.PatternFile, msgPrinter *message.Printer) (common.AbstractPatternFile, error) {
	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	if inputPat != nil {
		return inputPat, nil
	} else if patId != "" {
		if pattern, err := GetPattern(getPatterns, patId, msgPrinter); err != nil {
			return nil, err
		} else if pattern == nil {
			return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Pattern %v cannot be found on the exchange.", patId)), COMPCHECK_INPUT_ERROR)
		} else {
			p := Pattern{exchange.GetOrg(patId), *pattern}
			return &p, nil
		}
	} else {
		return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Neither pattern nor pattern id is specified.")), COMPCHECK_INPUT_ERROR)
	}
}

// get pattern from the exchange.
func GetPattern(getPatterns exchange.PatternHandler, patId string, msgPrinter *message.Printer) (*exchange.Pattern, error) {
	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	// check input
	if patId == "" {
		return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Pattern id is empty.")), COMPCHECK_INPUT_ERROR)
	} else {
		patOrg := exchange.GetOrg(patId)
		if patOrg == "" {
			return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Organization is not specified in the pattern id: %v.", patId)), COMPCHECK_INPUT_ERROR)
		}
	}

	// get pattern from the exchange
	exchPats, err := getPatterns(exchange.GetOrg(patId), exchange.GetId(patId))
	if err != nil {
		return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Error getting pattern %v from the exchange, %v", patId, err)), COMPCHECK_EXCHANGE_ERROR)
	}
	if exchPats == nil || len(exchPats) == 0 {
		return nil, NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("No pattern found for this id %v.", patId)), COMPCHECK_INPUT_ERROR)
	}

	for _, pat := range exchPats {
		pattern := pat
		return &pattern, nil
	}

	return nil, nil
}

// makes sure the input services are valid
func validateServices(inServices []common.AbstractServiceFile, bPolicy *businesspolicy.BusinessPolicy, pattern common.AbstractPatternFile, sIdsToCheck []string, msgPrinter *message.Printer) error {
	// get default message printer if nil
	if msgPrinter == nil {
		msgPrinter = i18n.GetMessagePrinter()
	}

	if inServices != nil && len(inServices) != 0 {
		for _, svc := range inServices {
			if svc.GetURL() == "" {
				return NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("URL must be specified in the service definition.")), COMPCHECK_VALIDATION_ERROR)
			}
			if svc.GetVersion() == "" {
				return NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Version must be specified in the service definition for service %v.", svc.GetURL())), COMPCHECK_VALIDATION_ERROR)
			} else if !semanticversion.IsVersionString(svc.GetVersion()) {
				return NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Invalide version format %v for service %v.", svc.GetVersion(), svc.GetURL())), COMPCHECK_VALIDATION_ERROR)
			}
			if svc.GetArch() == "" {
				return NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Arch must be specified in the service definition for service %v.", svc.GetURL())), COMPCHECK_VALIDATION_ERROR)
			}
			if svc.GetOrg() == "" {
				return NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Org must be specified in the service definition for service %v.", svc.GetURL())), COMPCHECK_VALIDATION_ERROR)
			}

			sId := cutil.FormExchangeIdForService(svc.GetURL(), svc.GetVersion(), svc.GetArch())
			sId = fmt.Sprintf("%v/%v", svc.GetOrg(), sId)
			if !needHandleService(sId, sIdsToCheck) {
				continue
			}

			var err error
			if bPolicy != nil {
				err = validateServiceWithBPolicy(svc, bPolicy, msgPrinter)
			} else {
				err = validateServiceWithPattern(svc, pattern, msgPrinter)
			}
			if err != nil {
				return NewCompCheckError(fmt.Errorf(msgPrinter.Sprintf("Validation failure for input service %v. %v", sId, err)), COMPCHECK_VALIDATION_ERROR)
			}
		}
	}

	return nil
}

// Get the service specified in the business policy and convert it into exchange.ServiceReference
// Only pick the ones with same arch as the given node arch.
func getWorkloadsFromBPol(bPolicy *businesspolicy.BusinessPolicy, nodeArch string) []exchange.ServiceReference {
	workloads := []exchange.ServiceReference{}
	sArch := bPolicy.Service.Arch
	if nodeArch != "" {
		if bPolicy.Service.Arch == "*" || bPolicy.Service.Arch == "" {
			sArch = nodeArch
		} else if nodeArch != bPolicy.Service.Arch {
			// not include the ones with different arch than the node arch
			return workloads
		}
	}

	versions := []exchange.WorkloadChoice{}
	if bPolicy.Service.ServiceVersions != nil {
		for _, v := range bPolicy.Service.ServiceVersions {
			// only add version in WorkloadChoice because this is what we are interested
			versions = append(versions, exchange.WorkloadChoice{Version: v.Version})
		}
	}
	// only inlucde ones with service version specified
	if len(versions) != 0 {
		wl := exchange.ServiceReference{ServiceURL: bPolicy.Service.Name, ServiceOrg: bPolicy.Service.Org, ServiceArch: sArch, ServiceVersions: versions}
		workloads = append(workloads, wl)
	}

	return workloads
}

// Get the services specified in the pattern.
// Only pick the ones with same arch as the given node arch.
func getWorkloadsFromPattern(pattern common.AbstractPatternFile, nodeArch string) []exchange.ServiceReference {
	workloads := []exchange.ServiceReference{}

	for _, svc := range pattern.GetServices() {
		if nodeArch != "" {
			if svc.ServiceArch == "*" || svc.ServiceArch == "" {
				svc.ServiceArch = nodeArch
			} else if nodeArch != svc.ServiceArch {
				//not include the ones with different arch from the node arch
				continue
			}
		}

		// only inlucde ones with service version specified
		if svc.ServiceVersions != nil && len(svc.ServiceVersions) != 0 {
			workloads = append(workloads, svc)
		}
	}
	return workloads
}
