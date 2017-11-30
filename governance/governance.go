package governance

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/abstractprotocol"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/ethblockchain"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/metering"
	"github.com/open-horizon/anax/microservice"
	"github.com/open-horizon/anax/persistence"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/producer"
	"github.com/open-horizon/anax/worker"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// TODO: make this module more aware of long-running setup operations like image downloading and dockerfile loading
// the max time we'll let a contract remain unconfigured by the provider
const MAX_CONTRACT_UNCONFIGURED_TIME_M = 20

const MAX_CONTRACT_PRELAUNCH_TIME_M = 10

const MAX_MICROPAYMENT_UNPAID_RUN_DURATION_M = 60

// enforced only after the workloads are running
const MAX_AGREEMENT_ACCEPTANCE_WAIT_TIME_M = 20

// related to agreement cleanup status
const STATUS_WORKLOAD_DESTROYED = 500
const STATUS_AG_PROTOCOL_TERMINATED = 501

// for identifying the subworkers used by this worker
const CONTAINER_GOVERNOR = "ContainerGovernor"
const MICROSERVICE_GOVERNOR = "MicroserviceGovernor"
const BC_GOVERNOR = "BlockchainGovernor"

type GovernanceWorker struct {
	worker.BaseWorker // embedded field
	db                *bolt.DB
	bc                *ethblockchain.BaseContracts
	deviceId          string
	deviceToken       string
	devicePattern     string
	pm                *policy.PolicyManager
	producerPH        map[string]producer.ProducerProtocolHandler
	deviceStatus      *DeviceStatus
	ShuttingDownCmd   *NodeShutdownCommand
	exchHandlers      *exchange.ExchangeApiHandlers
}

func NewGovernanceWorker(name string, cfg *config.HorizonConfig, db *bolt.DB, pm *policy.PolicyManager) *GovernanceWorker {

	id := ""
	token := ""
	pattern := ""
	if dev, _ := persistence.FindExchangeDevice(db); dev != nil {
		token = dev.Token
		id = fmt.Sprintf("%v/%v", dev.Org, dev.Id)
		pattern = dev.Pattern
	}

	worker := &GovernanceWorker{
		BaseWorker:      worker.NewBaseWorker(name, cfg),
		db:              db,
		pm:              pm,
		deviceId:        id,
		deviceToken:     token,
		devicePattern:   pattern,
		producerPH:      make(map[string]producer.ProducerProtocolHandler),
		deviceStatus:    NewDeviceStatus(),
		ShuttingDownCmd: nil,
		exchHandlers:    exchange.NewExchangeApiHandlers(cfg),
	}

	worker.Start(worker, 10)
	return worker
}

func (w *GovernanceWorker) Messages() chan events.Message {
	return w.BaseWorker.Manager.Messages
}

func (w *GovernanceWorker) NewEvent(incoming events.Message) {

	switch incoming.(type) {
	case *events.EdgeRegisteredExchangeMessage:
		msg, _ := incoming.(*events.EdgeRegisteredExchangeMessage)
		w.deviceId = fmt.Sprintf("%v/%v", msg.Org(), msg.DeviceId())
		w.deviceToken = msg.Token()
		w.devicePattern = msg.Pattern()

	case *events.WorkloadMessage:
		msg, _ := incoming.(*events.WorkloadMessage)

		switch msg.Event().Id {
		case events.EXECUTION_BEGUN:
			glog.Infof("Begun execution of containers according to agreement %v", msg.AgreementId)

			cmd := w.NewStartGovernExecutionCommand(msg.Deployment, msg.AgreementProtocol, msg.AgreementId)
			w.Commands <- cmd
		case events.EXECUTION_FAILED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, w.producerPH[msg.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_CONTAINER_FAILURE), msg.Deployment)
			w.Commands <- cmd
		case events.IMAGE_LOAD_FAILED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, w.producerPH[msg.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_WL_IMAGE_LOAD_FAILURE), msg.Deployment)
			w.Commands <- cmd
		case events.WORKLOAD_DESTROYED:
			cmd := w.NewCleanupStatusCommand(msg.AgreementProtocol, msg.AgreementId, STATUS_WORKLOAD_DESTROYED)
			w.Commands <- cmd
		}

		cmd := w.NewReportDeviceStatusCommand()
		w.Commands <- cmd

	case *events.TorrentMessage:
		msg, _ := incoming.(*events.TorrentMessage)

		// only handle the error case
		if msg.Event().Id != events.IMAGE_FETCHED {
			switch msg.LaunchContext.(type) {
			case *events.AgreementLaunchContext:
				var reason uint
				lc := msg.LaunchContext.(*events.AgreementLaunchContext)

				// get reason code from different image fetch error
				switch msg.Event().Id {
				case events.IMAGE_DATA_ERROR:
					reason = w.producerPH[lc.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_IMAGE_DATA_ERROR)
				case events.IMAGE_FETCH_ERROR:
					reason = w.producerPH[lc.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_IMAGE_FETCH_FAILURE)
				case events.IMAGE_FETCH_AUTH_ERROR:
					reason = w.producerPH[lc.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_IMAGE_FETCH_AUTH_FAILURE)
				case events.IMAGE_SIG_VERIF_ERROR:
					reason = w.producerPH[lc.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_IMAGE_SIG_VERIF_FAILURE)
				default:
					reason = w.producerPH[lc.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_IMAGE_FETCH_FAILURE)
				}
				cmd := w.NewCleanupExecutionCommand(lc.AgreementProtocol, lc.AgreementId, reason, nil)
				w.Commands <- cmd
			case *events.ContainerLaunchContext:
				lc := msg.LaunchContext.(*events.ContainerLaunchContext)
				cmd := w.NewUpdateMicroserviceCommand(lc.Name, false, microservice.MS_IMAGE_FETCH_FAILED, microservice.DecodeReasonCode(microservice.MS_IMAGE_FETCH_FAILED))
				w.Commands <- cmd
			}
		}

	case *events.InitAgreementCancelationMessage:
		msg, _ := incoming.(*events.InitAgreementCancelationMessage)
		switch msg.Event().Id {
		case events.AGREEMENT_ENDED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, msg.Reason, msg.Deployment)
			w.Commands <- cmd
		}
	case *events.ApiAgreementCancelationMessage:
		msg, _ := incoming.(*events.ApiAgreementCancelationMessage)
		switch msg.Event().Id {
		case events.AGREEMENT_ENDED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, w.producerPH[msg.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_USER_REQUESTED), msg.Deployment)
			w.Commands <- cmd
		}
	case *events.BlockchainClientInitializedMessage:
		msg, _ := incoming.(*events.BlockchainClientInitializedMessage)
		switch msg.Event().Id {
		case events.BC_CLIENT_INITIALIZED:
			cmd := producer.NewBCInitializedCommand(msg)
			w.Commands <- cmd
		}
	case *events.BlockchainClientStoppingMessage:
		msg, _ := incoming.(*events.BlockchainClientStoppingMessage)
		switch msg.Event().Id {
		case events.BC_CLIENT_STOPPING:
			cmd := producer.NewBCStoppingCommand(msg)
			w.Commands <- cmd
		}
	case *events.AccountFundedMessage:
		msg, _ := incoming.(*events.AccountFundedMessage)
		switch msg.Event().Id {
		case events.ACCOUNT_FUNDED:
			cmd := producer.NewBCWritableCommand(msg)
			w.Commands <- cmd
		}
	case *events.EthBlockchainEventMessage:
		msg, _ := incoming.(*events.EthBlockchainEventMessage)
		switch msg.Event().Id {
		case events.BC_EVENT:
			cmd := producer.NewBlockchainEventCommand(*msg)
			w.Commands <- cmd
		}
	case *events.ExchangeDeviceMessage:
		msg, _ := incoming.(*events.ExchangeDeviceMessage)
		switch msg.Event().Id {
		case events.RECEIVED_EXCHANGE_DEV_MSG:
			cmd := producer.NewExchangeMessageCommand(*msg)
			w.Commands <- cmd
		}

	case *events.ContainerMessage:
		msg, _ := incoming.(*events.ContainerMessage)
		if msg.LaunchContext.Blockchain.Name == "" { // microservice case
			switch msg.Event().Id {
			case events.EXECUTION_BEGUN:
				cmd := w.NewUpdateMicroserviceCommand(msg.LaunchContext.Name, true, 0, "")
				w.Commands <- cmd
			case events.EXECUTION_FAILED:
				cmd := w.NewUpdateMicroserviceCommand(msg.LaunchContext.Name, false, microservice.MS_EXEC_FAILED, microservice.DecodeReasonCode(microservice.MS_EXEC_FAILED))
				w.Commands <- cmd
			case events.IMAGE_LOAD_FAILED:
				cmd := w.NewUpdateMicroserviceCommand(msg.LaunchContext.Name, false, microservice.MS_IMAGE_LOAD_FAILED, microservice.DecodeReasonCode(microservice.MS_IMAGE_LOAD_FAILED))
				w.Commands <- cmd
			}

			cmd := w.NewReportDeviceStatusCommand()
			w.Commands <- cmd
		}
	case *events.MicroserviceContainersDestroyedMessage:
		msg, _ := incoming.(*events.MicroserviceContainersDestroyedMessage)

		switch msg.Event().Id {
		case events.CONTAINER_DESTROYED:
			cmd := w.NewUpdateMicroserviceCommand(msg.MsInstKey, false, 0, "")
			w.Commands <- cmd
		}

		cmd := w.NewReportDeviceStatusCommand()
		w.Commands <- cmd

	case *events.NodeShutdownMessage:

		msg, _ := incoming.(*events.NodeShutdownMessage)
		cmd := w.NewNodeShutdownCommand(msg)
		w.Commands <- cmd

	case *events.NodeShutdownCompleteMessage:
		msg, _ := incoming.(*events.NodeShutdownCompleteMessage)
		switch msg.Event().Id {
		case events.UNCONFIGURE_COMPLETE:
			w.Commands <- worker.NewTerminateCommand("shutdown")
		}

	default: //nothing
	}

	glog.V(4).Infof(logString(fmt.Sprintf("command channel length %v added", len(w.Commands))))

	return
}

func (w *GovernanceWorker) governAgreements() {

	glog.V(3).Infof(logString(fmt.Sprintf("governing pending agreements")))

	// Create a new filter for unfinalized agreements
	notYetFinalFilter := func() persistence.EAFilter {
		return func(a persistence.EstablishedAgreement) bool {
			return a.AgreementCreationTime != 0 && a.AgreementTerminatedTime == 0
		}
	}

	if establishedAgreements, err := persistence.FindEstablishedAgreementsAllProtocols(w.db, policy.AllAgreementProtocols(), []persistence.EAFilter{persistence.UnarchivedEAFilter(), notYetFinalFilter()}); err != nil {
		glog.Errorf(logString(fmt.Sprintf("Unable to retrieve not yet final agreements from database: %v. Error: %v", err, err)))
	} else {

		// If there are agreements in the database then we will assume that the device is already registered
		for _, ag := range establishedAgreements {
			bcType, bcName, bcOrg := w.producerPH[ag.AgreementProtocol].GetKnownBlockchain(&ag)
			protocolHandler := w.producerPH[ag.AgreementProtocol].AgreementProtocolHandler(bcType, bcName, bcOrg)
			if ag.AgreementFinalizedTime == 0 { // TODO: might need to change this to be a protocol specific check

				// Cancel the agreement if finalization doesn't occur before the timeout
				glog.V(5).Infof(logString(fmt.Sprintf("checking agreement %v for finalization.", ag.CurrentAgreementId)))

				// Check to see if we need to update the consumer with our blockchain specific pieces of the agreement
				if w.producerPH[ag.AgreementProtocol].IsBlockchainClientAvailable(bcType, bcName, bcOrg) && ag.AgreementBCUpdateAckTime == 0 {
					w.producerPH[ag.AgreementProtocol].UpdateConsumer(&ag)
				}

				// Check to see if the agreement is valid. For agreement on the blockchain, we check the blockchain directly. This call to the blockchain
				// should be very fast if the client is up and running. For other agreements, send a message to the agbot to get the agbot's opinion
				// on the agreement.
				// Remember, the device might have been down for some time and/or restarted, causing it to miss events on the blockchain.
				if w.producerPH[ag.AgreementProtocol].IsBlockchainClientAvailable(bcType, bcName, bcOrg) && w.producerPH[ag.AgreementProtocol].IsAgreementVerifiable(&ag) {

					if recorded, err := w.producerPH[ag.AgreementProtocol].VerifyAgreement(&ag); err != nil {
						glog.Errorf(logString(fmt.Sprintf("encountered error verifying agreement %v, error %v", ag.CurrentAgreementId, err)))
					} else if recorded {
						if err := w.finalizeAgreement(ag, protocolHandler); err != nil {
							glog.Errorf(err.Error())
						} else {
							continue
						}
					}
				}
				// If we fall through to here, then the agreement is Not finalized yet, check for a timeout.
				now := uint64(time.Now().Unix())
				if ag.AgreementCreationTime+w.BaseWorker.Manager.Config.Edge.AgreementTimeoutS < now {
					// Start timing out the agreement
					glog.V(3).Infof(logString(fmt.Sprintf("detected agreement %v timed out.", ag.CurrentAgreementId)))

					reason := w.producerPH[ag.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_NOT_FINALIZED_TIMEOUT)
					if ag.AgreementAcceptedTime == 0 {
						reason = w.producerPH[ag.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_NO_REPLY_ACK)
					}
					w.cancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, reason, w.producerPH[ag.AgreementProtocol].GetTerminationReason(reason))

					// cleanup workloads
					w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

					// clean up microservice instances if needed
					w.handleMicroserviceInstForAgEnded(ag.CurrentAgreementId, false)
				}

			} else {
				// For finalized agreements, make sure the workload has been started in time
				if ag.AgreementExecutionStartTime == 0 {
					// workload not started yet and in an agreement ...
					if (int64(ag.AgreementAcceptedTime) + (MAX_CONTRACT_PRELAUNCH_TIME_M * 60)) < time.Now().Unix() {
						glog.Infof(logString(fmt.Sprintf("terminating agreement %v because it hasn't been launched in max allowed time. This could be because of a workload failure.", ag.CurrentAgreementId)))
						reason := w.producerPH[ag.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_NOT_EXECUTED_TIMEOUT)
						w.cancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, reason, w.producerPH[ag.AgreementProtocol].GetTerminationReason(reason))
						// cleanup workloads if needed
						w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)
						// clean up microservice instances if needed
						w.handleMicroserviceInstForAgEnded(ag.CurrentAgreementId, false)
					}
				}
			}
		}
	}
}

// Make sure the workload containers are all running, by asking the container worker to verify.
func (w *GovernanceWorker) governContainers() int {

	// go govern
	glog.V(4).Infof(logString(fmt.Sprintf("governing containers")))

	// Create a new filter for unfinalized agreements
	runningFilter := func() persistence.EAFilter {
		return func(a persistence.EstablishedAgreement) bool {
			return a.AgreementExecutionStartTime != 0 && a.AgreementTerminatedTime == 0 && a.CounterPartyAddress != ""
		}
	}

	if establishedAgreements, err := persistence.FindEstablishedAgreementsAllProtocols(w.db, policy.AllAgreementProtocols(), []persistence.EAFilter{persistence.UnarchivedEAFilter(), runningFilter()}); err != nil {
		glog.Errorf(logString(fmt.Sprintf("Unable to retrieve running agreements from database, error: %v", err)))
	} else {
		for _, ag := range establishedAgreements {

			// Make sure containers are still running.
			glog.V(3).Infof(logString(fmt.Sprintf("fire event to ensure containers are still up for agreement %v.", ag.CurrentAgreementId)))

			// current contract, ensure workloads still running
			w.Messages() <- events.NewGovernanceMaintenanceMessage(events.CONTAINER_MAINTAIN, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

		}
	}
	return 0
}

func (w *GovernanceWorker) reportBlockchains() int {

	// go govern
	glog.Info(logString(fmt.Sprintf("started blockchain need governance")))

	// Find all agreements that need a blockchain by searching through all the agreement protocol DB buckets
	for _, agp := range policy.AllAgreementProtocols() {

		// If the agreement protocol doesnt require a blockchain then we can skip it.
		if bcType := policy.RequiresBlockchainType(agp); bcType == "" {
			continue
		} else {

			// Make a map of all blockchain orgs and names that we need to have running
			neededBCs := make(map[string]map[string]bool)
			if agreements, err := persistence.FindEstablishedAgreements(w.db, agp, []persistence.EAFilter{persistence.UnarchivedEAFilter()}); err == nil {
				for _, ag := range agreements {
					_, bcName, bcOrg := w.producerPH[agp].GetKnownBlockchain(&ag)
					if bcName != "" {
						if _, ok := neededBCs[bcOrg]; !ok {
							neededBCs[bcOrg] = make(map[string]bool)
						}
						neededBCs[bcOrg][bcName] = true
					}
				}

				// If we captured any needed blockchains, inform the blockchain worker
				if len(neededBCs) != 0 {
					w.Messages() <- events.NewReportNeededBlockchainsMessage(events.BC_NEEDED, bcType, neededBCs)
				}

			} else {
				glog.Errorf(logString(fmt.Sprintf("unable to read agreements from database for protocol %v, error: %v", agp, err)))
			}

		}
	}
	return 0
}

// It cancels the given agreement. Please take note that the system is very asynchronous. It is
// possible for multiple cancellations to occur in the time it takes to actually stop workloads and
// cancel on the blockchain, therefore this code needs to be prepared to run multiple times for the
// same agreement id.
func (w *GovernanceWorker) cancelAgreement(agreementId string, agreementProtocol string, reason uint, desc string) {

	// Update the database
	var ag *persistence.EstablishedAgreement
	if agreement, err := persistence.AgreementStateTerminated(w.db, agreementId, uint64(reason), desc, agreementProtocol); err != nil {
		glog.Errorf(logString(fmt.Sprintf("error marking agreement %v terminated: %v", agreementId, err)))
	} else {
		ag = agreement
	}

	// Delete from the exchange
	if ag != nil && ag.AgreementAcceptedTime != 0 {
		if err := deleteProducerAgreement(w.Config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken, agreementId); err != nil {
			glog.Errorf(logString(fmt.Sprintf("error deleting agreement %v in exchange: %v", agreementId, err)))
		}
	}

	// If we can do the termination now, do it. Otherwise we will queue a command to do it later.
	w.externalTermination(ag, agreementId, agreementProtocol, reason)
	if !w.producerPH[agreementProtocol].IsBlockchainWritable(ag) {
		// create deferred external termination command
		w.Commands <- NewAsyncTerminationCommand(agreementId, agreementProtocol, reason)
	}
}

func (w *GovernanceWorker) externalTermination(ag *persistence.EstablishedAgreement, agreementId string, agreementProtocol string, reason uint) {

	// Put the rest of the cancel processing into it's own go routine. In some agreement protocols, this go
	// routine will be waiting for a blockchain cancel to run. In general it will take around 30 seconds, but could be
	// double or triple that time. This will free up the governance thread to handle other protocol messages.

	// This routine does not need to be a subworker because it will terminate on its own.
	go func() {

		// Get the policy we used in the agreement and then cancel, just in case.
		glog.V(3).Infof(logString(fmt.Sprintf("terminating agreement %v", agreementId)))

		if ag != nil {
			w.producerPH[agreementProtocol].TerminateAgreement(ag, reason)
		}

		// report the cleanup status
		cmd := w.NewCleanupStatusCommand(agreementProtocol, agreementId, STATUS_AG_PROTOCOL_TERMINATED)
		w.Commands <- cmd
	}()

	// Concurrently write out the metering record. This is done in its own go routine for the same reason that
	// the blockchain cancel is done in a separate go routine. This go routine can complete after the agreement is
	// archived without any side effects.

	// This routine does not need to be a subworker because it will terminate on its own.
	go func() {
		// If there are metering notifications, write them onto the blockchain also
		if ag.MeteringNotificationMsg != (persistence.MeteringNotification{}) && !ag.Archived {
			glog.V(3).Infof(logString(fmt.Sprintf("Writing Metering Notification %v to the blockchain for %v.", ag.MeteringNotificationMsg, agreementId)))
			bcType, bcName, bcOrg := w.producerPH[agreementProtocol].GetKnownBlockchain(ag)
			if mn := metering.ConvertFromPersistent(ag.MeteringNotificationMsg, agreementId); mn == nil {
				glog.Errorf(logString(fmt.Sprintf("error converting from persistent Metering Notification %v for %v, returned nil.", ag.MeteringNotificationMsg, agreementId)))
			} else if aph := w.producerPH[agreementProtocol].AgreementProtocolHandler(bcType, bcName, bcOrg); aph == nil {
				glog.Warningf(logString(fmt.Sprintf("cannot write meter record for %v, agreement protocol handler is not ready.", agreementId)))
			} else if err := aph.RecordMeter(agreementId, mn); err != nil {
				glog.Errorf(logString(fmt.Sprintf("error writing meter %v for agreement %v on the blockchain: %v", ag.MeteringNotificationMsg, agreementId, err)))
			}
		}
	}()

}

func (w *GovernanceWorker) Initialize() bool {

	// Wait for the device to be registered.
	for {
		if w.deviceToken != "" {
			break
		} else {
			glog.V(3).Infof("GovernanceWorker command processor waiting for device registration")
			time.Sleep(time.Duration(5) * time.Second)
		}
	}

	// Establish agreement protocol handlers
	for _, protocolName := range policy.AllAgreementProtocols() {
		pph := producer.CreateProducerPH(protocolName, w.BaseWorker.Manager.Config, w.db, w.pm, w.deviceId, w.deviceToken)
		pph.Initialize()
		w.producerPH[protocolName] = pph
	}

	// report the device status to the exchange
	w.ReportDeviceStatus()

	// Fire up the container governor
	w.DispatchSubworker(CONTAINER_GOVERNOR, w.governContainers, 60)

	// Fire up the blockchain reporter
	w.DispatchSubworker(BC_GOVERNOR, w.reportBlockchains, 60)

	// Fire up the microservice governor
	w.DispatchSubworker(MICROSERVICE_GOVERNOR, w.governMicroservices, 60)

	return true

}

func (w *GovernanceWorker) CommandHandler(command worker.Command) bool {

	// Handle the domain specific commands
	// TODO: consolidate DB update cases
	switch command.(type) {
	case *StartGovernExecutionCommand:
		// TODO: update db start time and tc so it can be governed
		cmd, _ := command.(*StartGovernExecutionCommand)
		glog.V(3).Infof("Starting governance on resources in agreement: %v", cmd.AgreementId)

		if _, err := persistence.AgreementStateExecutionStarted(w.db, cmd.AgreementId, cmd.AgreementProtocol, &cmd.Deployment); err != nil {
			glog.Errorf("Failed to update local contract record to start governing Agreement: %v. Error: %v", cmd.AgreementId, err)
		}

	case *CleanupExecutionCommand:
		cmd, _ := command.(*CleanupExecutionCommand)

		agreementId := cmd.AgreementId
		if ags, err := persistence.FindEstablishedAgreements(w.db, cmd.AgreementProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(agreementId)}); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", agreementId, err)))
		} else if len(ags) != 1 {
			glog.V(5).Infof(logString(fmt.Sprintf("ignoring the event, unable to retrieve unarchived single agreement %v from the database.", agreementId)))
		} else if ags[0].AgreementTerminatedTime != 0 && ags[0].AgreementForceTerminatedTime == 0 {
			glog.V(3).Infof(logString(fmt.Sprintf("ignoring the event, agreement %v is already terminating", agreementId)))
		} else {
			glog.V(3).Infof("Ending the agreement: %v", agreementId)
			w.cancelAgreement(agreementId, cmd.AgreementProtocol, cmd.Reason, w.producerPH[cmd.AgreementProtocol].GetTerminationReason(cmd.Reason))

			// send the event to the container in case it has started the workloads.
			w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, cmd.AgreementProtocol, agreementId, cmd.Deployment)
			// clean up microservice instances if needed
			w.handleMicroserviceInstForAgEnded(agreementId, false)
		}

	case *producer.ExchangeMessageCommand:
		cmd, _ := command.(*producer.ExchangeMessageCommand)

		exchangeMsg := new(exchange.DeviceMessage)
		if err := json.Unmarshal(cmd.Msg.ExchangeMessage(), &exchangeMsg); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to demarshal exchange device message %v, error %v", cmd.Msg.ExchangeMessage(), err)))
			return true
		} else if there, err := w.messageInExchange(exchangeMsg.MsgId); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to get messages from the exchange, error %v", err)))
			return true
		} else if !there {
			glog.V(3).Infof(logString(fmt.Sprintf("ignoring message %v, already deleted from the exchange.", exchangeMsg.MsgId)))
			return true
		}

		glog.V(3).Infof(logString(fmt.Sprintf("received message %v from the exchange", exchangeMsg.MsgId)))

		deleteMessage := true
		protocolMsg := cmd.Msg.ProtocolMessage()

		// Pull the agreement protocol out of the message
		if msgProtocol, err := abstractprotocol.ExtractProtocol(protocolMsg); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to extract agreement protocol name from message %v", protocolMsg)))
		} else if _, ok := w.producerPH[msgProtocol]; !ok {
			glog.Infof(logString(fmt.Sprintf("unable to direct exchange message %v to a protocol handler, deleting it.", protocolMsg)))
		} else {

			deleteMessage = false
			protocolHandler := w.producerPH[msgProtocol].AgreementProtocolHandler("", "", "")
			// ReplyAck messages could indicate that the agbot has decided not to pursue the agreement any longer.
			if replyAck, err := protocolHandler.ValidateReplyAck(protocolMsg); err != nil {
				glog.V(5).Infof(logString(fmt.Sprintf("ReplyAck handler ignoring non-reply ack message: %s due to %v", cmd.Msg.ShortProtocolMessage(), err)))
			} else if ags, err := persistence.FindEstablishedAgreements(w.db, msgProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(replyAck.AgreementId())}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", replyAck.AgreementId(), err)))
			} else if len(ags) != 1 {
				glog.Warningf(logString(fmt.Sprintf("unable to retrieve single agreement %v from database.", replyAck.AgreementId())))
				deleteMessage = true
			} else if replyAck.ReplyAgreementStillValid() {
				if ags[0].AgreementAcceptedTime != 0 || ags[0].AgreementTerminatedTime != 0 {
					glog.V(5).Infof(logString(fmt.Sprintf("ignoring replyack for %v because we already received one or are cancelling", replyAck.AgreementId())))
					deleteMessage = true
				} else if proposal, err := protocolHandler.DemarshalProposal(ags[0].Proposal); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to demarshal proposal for agreement %v from database", replyAck.AgreementId())))
				} else if err := w.RecordReply(proposal, msgProtocol); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to record reply %v, error: %v", replyAck, err)))
				} else {
					deleteMessage = true
				}
			} else {
				deleteMessage = true
				w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ags[0].AgreementProtocol, ags[0].CurrentAgreementId, ags[0].CurrentDeployment)
				reason := w.producerPH[msgProtocol].GetTerminationCode(producer.TERM_REASON_AGBOT_REQUESTED)
				w.cancelAgreement(replyAck.AgreementId(), msgProtocol, reason, w.producerPH[msgProtocol].GetTerminationReason(reason))
				// clean up microservice instances if needed
				w.handleMicroserviceInstForAgEnded(replyAck.AgreementId(), false)
			}

			// Data notification message indicates that the agbot has found that data is being received from the workload.
			if dataReceived, err := protocolHandler.ValidateDataReceived(protocolMsg); err != nil {
				glog.V(5).Infof(logString(fmt.Sprintf("DataReceived handler ignoring non-data received message: %v due to %v", cmd.Msg.ShortProtocolMessage(), err)))
			} else if ags, err := persistence.FindEstablishedAgreements(w.db, msgProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(dataReceived.AgreementId())}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", dataReceived.AgreementId(), err)))
			} else if len(ags) != 1 {
				glog.Warningf(logString(fmt.Sprintf("unable to retrieve single agreement %v from database, error %v", dataReceived.AgreementId(), err)))
				deleteMessage = true
			} else if _, err := persistence.AgreementStateDataReceived(w.db, dataReceived.AgreementId(), msgProtocol); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to update data received time for %v, error: %v", dataReceived.AgreementId(), err)))
			} else if messageTarget, err := exchange.CreateMessageTarget(exchangeMsg.AgbotId, nil, exchangeMsg.AgbotPubKey, ""); err != nil {
				glog.Errorf(logString(fmt.Sprintf("error creating message target: %v", err)))
			} else if err := protocolHandler.NotifyDataReceiptAck(dataReceived.AgreementId(), messageTarget, w.producerPH[msgProtocol].GetSendMessage()); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to send data received ack for %v, error: %v", dataReceived.AgreementId(), err)))
			} else {
				deleteMessage = true
			}

			// Metering notification messages indicate that the agbot is metering data sent to the data ingest.
			if mnReceived, err := protocolHandler.ValidateMeterNotification(protocolMsg); err != nil {
				glog.V(5).Infof(logString(fmt.Sprintf("Meter Notification handler ignoring non-metering message: %v due to %v", cmd.Msg.ShortProtocolMessage(), err)))
			} else if ags, err := persistence.FindEstablishedAgreements(w.db, msgProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(mnReceived.AgreementId())}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", mnReceived.AgreementId(), err)))
			} else if len(ags) != 1 {
				glog.Warningf(logString(fmt.Sprintf("unable to retrieve single agreement %v from database, error %v", mnReceived.AgreementId(), err)))
				deleteMessage = true
			} else if ags[0].AgreementTerminatedTime != 0 {
				glog.V(5).Infof(logString(fmt.Sprintf("ignoring metering notification, agreement %v is terminating", mnReceived.AgreementId())))
				deleteMessage = true
			} else if mn, err := metering.ConvertToPersistent(mnReceived.Meter()); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to convert metering notification string %v to persistent metering notification for %v, error: %v", mnReceived.Meter(), mnReceived.AgreementId(), err)))
				deleteMessage = true
			} else if _, err := persistence.MeteringNotificationReceived(w.db, mnReceived.AgreementId(), *mn, msgProtocol); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to update metering notification for %v, error: %v", mnReceived.AgreementId(), err)))
				deleteMessage = true
			} else {
				deleteMessage = true
			}

			// Cancel messages indicate that the agbot wants to get rid of the agreement.
			if canReceived, err := protocolHandler.ValidateCancel(protocolMsg); err != nil {
				glog.V(5).Infof(logString(fmt.Sprintf("Cancel handler ignoring non-cancel message: %v due to %v", cmd.Msg.ShortProtocolMessage(), err)))
			} else if ags, err := persistence.FindEstablishedAgreements(w.db, msgProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(canReceived.AgreementId())}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", canReceived.AgreementId(), err)))
			} else if len(ags) != 1 {
				glog.Warningf(logString(fmt.Sprintf("unable to retrieve single agreement %v from database, error %v", canReceived.AgreementId(), err)))
				deleteMessage = true
			} else if exchangeMsg.AgbotId != ags[0].ConsumerId {
				glog.Warningf(logString(fmt.Sprintf("cancel ignored, cancel message for %v came from id %v but agreement is with %v", canReceived.AgreementId(), exchangeMsg.AgbotId, ags[0].ConsumerId)))
				deleteMessage = true
			} else if ags[0].AgreementTerminatedTime != 0 {
				glog.V(5).Infof(logString(fmt.Sprintf("ignoring cancel, agreement %v is terminating", canReceived.AgreementId())))
				deleteMessage = true
			} else {
				w.cancelAgreement(canReceived.AgreementId(), msgProtocol, canReceived.Reason(), w.producerPH[msgProtocol].GetTerminationReason(canReceived.Reason()))
				// cleanup workloads if needed
				w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ags[0].AgreementProtocol, ags[0].CurrentAgreementId, ags[0].CurrentDeployment)
				// clean up microservice instances if needed
				w.handleMicroserviceInstForAgEnded(ags[0].CurrentAgreementId, false)
				deleteMessage = true

			}

			// Allow the message extension handler to see the message
			handled, cancel, agid, err := w.producerPH[msgProtocol].HandleExtensionMessages(&cmd.Msg, exchangeMsg)
			if err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to handle extension message %v , error: %v", protocolMsg, err)))
			}
			if cancel {
				reason := w.producerPH[msgProtocol].GetTerminationCode(producer.TERM_REASON_AGBOT_REQUESTED)

				if ags, err := persistence.FindEstablishedAgreements(w.db, msgProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(agid)}); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", agid, err)))
				} else if len(ags) != 1 {
					glog.Warningf(logString(fmt.Sprintf("unable to retrieve single agreement %v from database, error %v", agid, err)))
					deleteMessage = true
				} else {
					w.cancelAgreement(agid, msgProtocol, reason, w.producerPH[msgProtocol].GetTerminationReason(reason))
					// cleanup workloads if needed
					w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, msgProtocol, agid, ags[0].CurrentDeployment)
					// clean up microservice instances if needed
					w.handleMicroserviceInstForAgEnded(agid, false)
				}
			}
			if handled {
				deleteMessage = handled
			}

		}

		// Get rid of the exchange message when we're done with it
		if deleteMessage {
			if err := w.deleteMessage(exchangeMsg); err != nil {
				glog.Errorf(logString(fmt.Sprintf("error deleting exchange message %v, error %v", exchangeMsg.MsgId, err)))
			}
		}

	case *producer.BlockchainEventCommand:
		cmd, _ := command.(*producer.BlockchainEventCommand)

		for _, protocol := range policy.AllAgreementProtocols() {
			if !w.producerPH[protocol].AcceptCommand(cmd) {
				continue
			}

			if agreementId, termination, reason, creation, err := w.producerPH[protocol].HandleBlockchainEventMessage(cmd); err != nil {
				glog.Errorf(err.Error())
			} else if termination {

				// If we have that agreement in our DB, then cancel it
				if ags, err := persistence.FindEstablishedAgreements(w.db, protocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(agreementId)}); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", agreementId, err)))
				} else if len(ags) != 1 {
					glog.V(5).Infof(logString(fmt.Sprintf("ignoring event, not our agreement id")))
				} else if ags[0].AgreementTerminatedTime != 0 {
					glog.V(5).Infof(logString(fmt.Sprintf("ignoring event, agreement %v is already terminating", ags[0].CurrentAgreementId)))
				} else {
					glog.Infof(logString(fmt.Sprintf("terminating agreement %v because it has been cancelled on the blockchain.", ags[0].CurrentAgreementId)))
					w.cancelAgreement(ags[0].CurrentAgreementId, ags[0].AgreementProtocol, uint(reason), w.producerPH[protocol].GetTerminationReason(uint(reason)))
					// cleanup workloads if needed
					w.Messages() <- events.NewGovernanceWorkloadCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ags[0].AgreementProtocol, ags[0].CurrentAgreementId, ags[0].CurrentDeployment)
					// clean up microservice instances if needed
					w.handleMicroserviceInstForAgEnded(ags[0].CurrentAgreementId, false)
				}

				// If the event is an agreement created event
			} else if creation {

				// If we have that agreement in our DB and it's not already terminating, then finalize it
				if ags, err := persistence.FindEstablishedAgreements(w.db, protocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(agreementId)}); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", agreementId, err)))
				} else if len(ags) != 1 {
					glog.V(5).Infof(logString(fmt.Sprintf("ignoring event, not our agreement id")))
				} else if ags[0].AgreementTerminatedTime != 0 {
					glog.V(5).Infof(logString(fmt.Sprintf("ignoring event, agreement %v is terminating", ags[0].CurrentAgreementId)))

					// Finalize the agreement
				} else if err := w.finalizeAgreement(ags[0], w.producerPH[protocol].AgreementProtocolHandler(ags[0].BlockchainType, ags[0].BlockchainName, ags[0].BlockchainOrg)); err != nil {
					glog.Errorf(err.Error())
				}
			}
		}

	case *CleanupStatusCommand:
		cmd, _ := command.(*CleanupStatusCommand)

		glog.V(5).Infof(logString(fmt.Sprintf("Received CleanupStatusCommand: %v.", cmd)))
		if ags, err := persistence.FindEstablishedAgreements(w.db, cmd.AgreementProtocol, []persistence.EAFilter{persistence.UnarchivedEAFilter(), persistence.IdEAFilter(cmd.AgreementId)}); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", cmd.AgreementId, err)))
		} else if len(ags) != 1 {
			glog.V(5).Infof(logString(fmt.Sprintf("ignoring event, not our agreement id")))
		} else if ags[0].AgreementAcceptedTime == 0 {
			// The only place the agreement is known is in the DB, so we can just delete the record. In the situation where
			// the agbot changes its mind about the proposal, we don't want to create an archived agreement because an
			// agreement was never really established.
			if err := persistence.DeleteEstablishedAgreement(w.db, cmd.AgreementId, cmd.AgreementProtocol); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to delete record for agreement %v, error: %v", cmd.AgreementId, err)))
			}
		} else {
			// writes the cleanup status into the db
			var archive = false
			switch cmd.Status {
			case STATUS_WORKLOAD_DESTROYED:
				if agreement, err := persistence.AgreementStateWorkloadTerminated(w.db, cmd.AgreementId, cmd.AgreementProtocol); err != nil {
					glog.Errorf(logString(fmt.Sprintf("error marking agreement %v workload terminated: %v", cmd.AgreementId, err)))
				} else if agreement.AgreementProtocolTerminatedTime != 0 {
					archive = true
				}
			case STATUS_AG_PROTOCOL_TERMINATED:
				if agreement, err := persistence.AgreementStateAgreementProtocolTerminated(w.db, cmd.AgreementId, cmd.AgreementProtocol); err != nil {
					glog.Errorf(logString(fmt.Sprintf("error marking agreement %v agreement protocol terminated: %v", cmd.AgreementId, err)))
				} else if agreement.WorkloadTerminatedTime != 0 {
					archive = true
				}
			default:
				glog.Errorf(logString(fmt.Sprintf("The cleanup status %v is not supported for agreement %v.", cmd.Status, cmd.AgreementId)))
			}

			// archive the agreement if all the cleanup processes are done
			if archive {
				glog.V(5).Infof(logString(fmt.Sprintf("archiving agreement %v", cmd.AgreementId)))
				if _, err := persistence.ArchiveEstablishedAgreement(w.db, cmd.AgreementId, cmd.AgreementProtocol); err != nil {
					glog.Errorf(logString(fmt.Sprintf("error archiving terminated agreement: %v, error: %v", cmd.AgreementId, err)))
				}
			}
		}

	case *producer.BCInitializedCommand:
		cmd, _ := command.(*producer.BCInitializedCommand)
		for _, pph := range w.producerPH {
			pph.SetBlockchainClientAvailable(cmd)
		}

	case *producer.BCStoppingCommand:
		cmd, _ := command.(*producer.BCStoppingCommand)
		for _, pph := range w.producerPH {
			pph.SetBlockchainClientNotAvailable(cmd)
		}

	case *producer.BCWritableCommand:
		cmd, _ := command.(*producer.BCWritableCommand)
		for _, pph := range w.producerPH {
			pph.SetBlockchainWritable(cmd)
			pph.UpdateConsumers()
		}

	case *AsyncTerminationCommand:
		cmd, _ := command.(*AsyncTerminationCommand)
		if ags, err := persistence.FindEstablishedAgreements(w.db, cmd.AgreementProtocol, []persistence.EAFilter{persistence.IdEAFilter(cmd.AgreementId)}); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to retrieve agreement %v from database, error %v", cmd.AgreementId, err)))
		} else if len(ags) != 1 {
			glog.V(5).Infof(logString(fmt.Sprintf("ignoring command, not our agreement id")))
		} else if w.producerPH[cmd.AgreementProtocol].IsBlockchainWritable(&ags[0]) {
			glog.Infof(logString(fmt.Sprintf("external agreement termination of %v reason %v.", cmd.AgreementId, cmd.Reason)))
			w.externalTermination(&ags[0], cmd.AgreementId, cmd.AgreementProtocol, cmd.Reason)
		} else {
			w.AddDeferredCommand(cmd)
		}
	case *UpdateMicroserviceCommand:
		cmd, _ := command.(*UpdateMicroserviceCommand)

		glog.V(5).Infof(logString(fmt.Sprintf("Updating microservice execution status %v", cmd)))

		if cmd.ExecutionStarted == false && cmd.ExecutionFailureCode == 0 {
			// the miceroservice containers were destroyed, just archive the ms instance it if it not already done
			// this part is from the CONTAINER_DESTROYED event id which was originally
			if _, err := persistence.ArchiveMicroserviceInstance(w.db, cmd.MsInstKey); err != nil {
				glog.Errorf(logString(fmt.Sprintf("Error archiving microservice instance %v. %v", cmd.MsInstKey, err)))
			}
		} else {
			// microservice execution started or failed
			// this part is from EXECUTION_FAILED or EXECUTION_BEGUN event id

			// update the execution status for microservice instance
			if msinst, err := persistence.UpdateMSInstanceExecutionState(w.db, cmd.MsInstKey, cmd.ExecutionStarted, cmd.ExecutionFailureCode, cmd.ExecutionFailureDesc); err != nil {
				glog.Errorf(logString(fmt.Sprintf("Error updating microservice execution status. %v", err)))
			} else if msinst != nil {
				if msdef, err := persistence.FindMicroserviceDefWithKey(w.db, msinst.MicroserviceDefId); err != nil {
					glog.Errorf(logString(fmt.Sprintf("Error finding microserivce definition fron db for %v version %v key %v. %v", msinst.SpecRef, msinst.Version, msinst.MicroserviceDefId, err)))
				} else if msdef == nil {
					glog.Errorf(logString(fmt.Sprintf("No microserivce definition record in db for %v version %v key %v. %v", msinst.SpecRef, msinst.Version, msinst.MicroserviceDefId, err)))
				} else {
					if msdef.UpgradeStartTime != 0 && msdef.UpgradeExecutionStartTime == 0 && msdef.UpgradeFailedTime == 0 {
						// handle the rest of the microservice upgrade process
						w.handleMicroserviceUpgradeExecStateChange(msdef, cmd.MsInstKey, cmd.ExecutionStarted)
					} else if !cmd.ExecutionStarted && msinst.CleanupStartTime == 0 { // if this is not part of the ms instance cleanup process
						// this is the case where agreement are made but microservice containers are failed
						w.handleMicroserviceExecFailure(msdef, cmd.MsInstKey)
					}
				}
			}
		}
	case *ReportDeviceStatusCommand:
		cmd, _ := command.(*ReportDeviceStatusCommand)

		glog.V(5).Infof(logString(fmt.Sprintf("Report device status command %v", cmd)))
		w.ReportDeviceStatus()

	case *NodeShutdownCommand:
		cmd, _ := command.(*NodeShutdownCommand)
		glog.V(5).Infof(logString(fmt.Sprintf("Node shutdown command %v", cmd)))

		// Remember the command until we need it again.
		w.SetWorkerShuttingDown()
		w.ShuttingDownCmd = cmd

		// Shutdown the governance subworkers. We do this to ensure that none of them wake up to do
		// something when we're shutting down (which could cause problems) because we dont need them
		// to complete the shutdown procedure.
		w.TerminateSubworkers()

	default:
		return false
	}
	return true
}

func (w *GovernanceWorker) NoWorkHandler() {

	// Make sure that all known agreements are maintained, if we're not shutting down.
	if !w.IsWorkerShuttingDown() {
		w.governAgreements()
	}

	// When all subworkers are down, start the shutdown process.
	if w.IsWorkerShuttingDown() && w.ShuttingDownCmd != nil {
		if w.AreAllSubworkersTerminated() {
			cmd := w.ShuttingDownCmd
			// This is one of the few go routines that should NOT be abstracted as a subworker.
			go w.nodeShutdown(cmd)
			w.ShuttingDownCmd = nil
		} else {
			glog.V(5).Infof("GovernanceWorker waiting for subworkers to terminate.")
		}
	}

}

// This function encapsulates finalization of an agreement for re-use
func (w *GovernanceWorker) finalizeAgreement(agreement persistence.EstablishedAgreement, protocolHandler abstractprotocol.ProtocolHandler) error {

	// The reply ack might have been lost or mishandled. Since we are now seeing evidence on the blockchain that the agreement
	// was created by the agbot, we will assume we should have gotten a positive reply ack.
	if agreement.AgreementAcceptedTime == 0 {
		if proposal, err := protocolHandler.DemarshalProposal(agreement.Proposal); err != nil {
			return errors.New(logString(fmt.Sprintf("unable to demarshal proposal for agreement %v from database, error %v", agreement.CurrentAgreementId, err)))
		} else if err := w.RecordReply(proposal, protocolHandler.Name()); err != nil {
			return errors.New(logString(fmt.Sprintf("unable to accept agreement %v, error: %v", agreement.CurrentAgreementId, err)))
		}
	}

	// Finalize the agreement in the DB
	if _, err := persistence.AgreementStateFinalized(w.db, agreement.CurrentAgreementId, protocolHandler.Name()); err != nil {
		return errors.New(logString(fmt.Sprintf("error persisting agreement %v finalized: %v", agreement.CurrentAgreementId, err)))
	} else {
		glog.V(3).Infof(logString(fmt.Sprintf("agreement %v finalized", agreement.CurrentAgreementId)))
	}

	// Update state in exchange
	if proposal, err := protocolHandler.DemarshalProposal(agreement.Proposal); err != nil {
		return errors.New(logString(fmt.Sprintf("could not hydrate proposal, error: %v", err)))
	} else if tcPolicy, err := policy.DemarshalPolicy(proposal.TsAndCs()); err != nil {
		return errors.New(logString(fmt.Sprintf("error demarshalling TsAndCs policy for agreement %v, error %v", agreement.CurrentAgreementId, err)))
	} else if err := recordProducerAgreementState(w.Config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken, w.devicePattern, agreement.CurrentAgreementId, tcPolicy, "Finalized Agreement"); err != nil {
		return errors.New(logString(fmt.Sprintf("error setting agreement %v finalized state in exchange: %v", agreement.CurrentAgreementId, err)))
	}

	return nil
}

func (w *GovernanceWorker) RecordReply(proposal abstractprotocol.Proposal, protocol string) error {

	// Update the state in the database
	if ag, err := persistence.AgreementStateAccepted(w.db, proposal.AgreementId(), protocol); err != nil {
		return errors.New(logString(fmt.Sprintf("received error updating database state, %v", err)))

		// Update the state in the exchange
	} else if tcPolicy, err := policy.DemarshalPolicy(proposal.TsAndCs()); err != nil {
		return errors.New(logString(fmt.Sprintf("received error demarshalling TsAndCs, %v", err)))
	} else if err := recordProducerAgreementState(w.Config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken, w.devicePattern, proposal.AgreementId(), tcPolicy, "Agree to proposal"); err != nil {
		return errors.New(logString(fmt.Sprintf("received error setting state for agreement %v", err)))
	} else {
		// Publish the "agreement reached" event to the message bus so that torrent can start downloading the workload
		// hash is same as filename w/out extension
		workload := tcPolicy.NextHighestPriorityWorkload(0, 0, 0)
		if url, err := url.Parse(workload.Torrent.Url); err != nil {
			return errors.New(fmt.Sprintf("Ill-formed URL: %v", workload.Torrent.Url))
		} else {
			cc := events.NewContainerConfig(*url, workload.Torrent.Signature, workload.Deployment, workload.DeploymentSignature, workload.DeploymentUserInfo, workload.DeploymentOverrides)

			lc := new(events.AgreementLaunchContext)
			lc.Configure = *cc
			lc.AgreementId = proposal.AgreementId()

			// get environmental settings for the workload
			envAdds := make(map[string]string)

			// Before the ms split, the attributes assigned to the service (sensorUrl) are added to the workload.
			// After the split, the workload config variables are stored in the workload config database.
			if workload.WorkloadURL == "" {
				sensorUrl := tcPolicy.APISpecs[0].SpecRef
				if envAdds, err = w.GetWorkloadPreference(sensorUrl); err != nil {
					glog.Errorf("Error: %v", err)
					return err
				}
			} else {
				if envAdds, err = w.GetWorkloadConfig(workload.WorkloadURL, workload.Version); err != nil {
					glog.Errorf("Error: %v", err)
					return err
				}
				// The workload config we have might be from a lower version of the workload. Go to the exchange and
				// get the metadata for the version we are running and then add in any unset default user inputs.
				if exWkld, err := exchange.GetWorkload(w.Config.Collaborators.HTTPClientFactory, workload.WorkloadURL, workload.Org, workload.Version, workload.Arch, w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken); err != nil {
					return errors.New(logString(fmt.Sprintf("received error querying excahnge for workload metadata, error %v", err)))
				} else {
					for _, ui := range exWkld.UserInputs {
						if ui.DefaultValue != "" {
							if _, ok := envAdds[ui.Name]; !ok {
								envAdds[ui.Name] = ui.DefaultValue
							}
						}
					}
				}
			}

			envAdds[config.ENVVAR_PREFIX+"AGREEMENTID"] = proposal.AgreementId()
			envAdds[config.ENVVAR_PREFIX+"DEVICE_ID"] = exchange.GetId(w.deviceId)
			envAdds[config.ENVVAR_PREFIX+"ORGANIZATION"] = exchange.GetOrg(w.deviceId)
			envAdds[config.ENVVAR_PREFIX+"HASH"] = workload.WorkloadPassword

			// Add in the exchange URL so that the workload knows which ecosystem its part of
			envAdds[config.ENVVAR_PREFIX+"EXCHANGE_URL"] = w.Config.Edge.ExchangeURL

			lc.EnvironmentAdditions = &envAdds
			lc.AgreementProtocol = protocol

			// get a list of microservices associated with this agreement and store them in the AgreementLaunchContext
			ms_specs := []events.MicroserviceSpec{}
			for _, as := range tcPolicy.APISpecs {
				// find the msdef with the url, any version.
				if msdefs, err := persistence.FindUnarchivedMicroserviceDefs(w.db, as.SpecRef); err != nil {
					return errors.New(logString(fmt.Sprintf("Error finding microservice definition from the local db for %v version range %v. %v", as.SpecRef, as.Version, err)))
				} else if msdefs != nil && len(msdefs) > 0 { // if msdefs is nil or empty then it is old behaviour before the ms split
					glog.V(5).Infof("All avaialbe msdefs: %v", msdefs)
					// assuming there is only one msdef for a microservice at any time
					msdef := msdefs[0]

					// validate the version range
					if vExp, err := policy.Version_Expression_Factory(as.Version); err != nil {
						return errors.New(logString(fmt.Sprintf("Error converting APISpec version %v for %v to version range. %v", as.Version, as.SpecRef, err)))
					} else if inRange, err := vExp.Is_within_range(msdef.Version); err != nil {
						return errors.New(logString(fmt.Sprintf("Error checking if microservice version %v is within APISpec version range %v for %v. %v", msdef.Version, vExp, as.SpecRef, err)))
					} else if !inRange {
						return errors.New(logString(fmt.Sprintf("Current microservice %v version %v is not within the APISpec version range %v. %v", msdef.SpecRef, msdef.Version, vExp, err)))
					}

					// here we change to single version and choose a specific msdef for the container
					msspec := events.MicroserviceSpec{SpecRef: msdef.SpecRef, Version: msdef.Version, MsdefId: msdef.Id}
					ms_specs = append(ms_specs, msspec)

					// now we can start the microservice
					if err := w.startMicroserviceInstForAgreement(&msdef, proposal.AgreementId(), protocol); err != nil {
						return errors.New(logString(fmt.Sprintf("Failed to start microservice instance for %v version %v key %v. %v", msdef.SpecRef, msdef.Version, msdef.Id, err)))
					}

				}
			}
			lc.Microservices = ms_specs

			w.BaseWorker.Manager.Messages <- events.NewAgreementMessage(events.AGREEMENT_REACHED, lc)
		}

		// Tell the BC worker to start the BC client container(s) if we need to.
		if ag.BlockchainType != "" && ag.BlockchainName != "" && ag.BlockchainOrg != "" {
			w.BaseWorker.Manager.Messages <- events.NewNewBCContainerMessage(events.NEW_BC_CLIENT, ag.BlockchainType, ag.BlockchainName, ag.BlockchainOrg, w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken)
		}
	}

	return nil
}

// get the environmental variables for the workload (this is about launching), pre MS split
func (w *GovernanceWorker) GetWorkloadPreference(url string) (map[string]string, error) {
	attrs, err := persistence.FindApplicableAttributes(w.db, url)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch workload preferences. Err: %v", err)
	}

	return persistence.AttributesToEnvvarMap(attrs, config.ENVVAR_PREFIX)

}

// Get the environmental variables for the workload for MS split workloads. The workload config
// record has a version range. The workload has to fall within that range inorder for us to apply
// the configuration to the workload. If there are multiple configs in the version range, we will
// use the most current config we have.
func (w *GovernanceWorker) GetWorkloadConfig(url string, version string) (map[string]string, error) {

	// Filter to return workload configs with versions less than or equal to the input workload version range
	OlderWorkloadWCFilter := func(workload_url string, version string) persistence.WCFilter {
		return func(e persistence.WorkloadConfigOnly) bool {
			if vExp, err := policy.Version_Expression_Factory(e.VersionExpression); err != nil {
				return false
			} else if inRange, err := vExp.Is_within_range(version); err != nil {
				return false
			} else if e.WorkloadURL == workload_url && inRange {
				return true
			} else {
				return false
			}
		}
	}

	// Find the eligible workload config objects
	cfgs, err := persistence.FindWorkloadConfigs(w.db, []persistence.WCFilter{OlderWorkloadWCFilter(url, version)})
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch post split workload preferences. Err: %v", err)
	} else if len(cfgs) == 0 {
		return persistence.ConfigToEnvvarMap(w.db, nil, config.ENVVAR_PREFIX)
	}

	// Sort them by version, oldest to newest
	sort.Sort(persistence.WorkloadConfigByVersion(cfgs))

	// Configure the env map with the newest config that is within the version range.
	return persistence.ConfigToEnvvarMap(w.db, &cfgs[len(cfgs)-1], config.ENVVAR_PREFIX)

}

func recordProducerAgreementState(httpClient *http.Client, url string, deviceId string, token string, pattern string, agreementId string, pol *policy.Policy, state string) error {

	glog.V(5).Infof(logString(fmt.Sprintf("setting agreement %v state to %v", agreementId, state)))

	as := new(exchange.PutAgreementState)
	for _, apiSpec := range pol.APISpecs {
		as.Microservices = append(as.Microservices, exchange.MSAgreementState{
			Org: apiSpec.Org,
			URL: apiSpec.SpecRef,
		})
	}

	if pattern != "" {
		as.Workload = exchange.WorkloadAgreement{
			Org:     exchange.GetOrg(deviceId),
			Pattern: pattern,
			URL:     pol.Workloads[0].WorkloadURL,
		}
	}

	as.State = state

	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := url + "orgs/" + exchange.GetOrg(deviceId) + "/nodes/" + exchange.GetId(deviceId) + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(httpClient, "PUT", targetURL, deviceId, token, &as, &resp); err != nil {
			glog.Errorf(logString(fmt.Sprintf(err.Error())))
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(5).Infof(logString(fmt.Sprintf("set agreement %v to state %v", agreementId, state)))
			return nil
		}
	}

}

func deleteProducerAgreement(httpClient *http.Client, url string, deviceId string, token string, agreementId string) error {

	glog.V(5).Infof(logString(fmt.Sprintf("deleting agreement %v in exchange", agreementId)))

	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := url + "orgs/" + exchange.GetOrg(deviceId) + "/nodes/" + exchange.GetId(deviceId) + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(httpClient, "DELETE", targetURL, deviceId, token, nil, &resp); err != nil {
			glog.Errorf(logString(fmt.Sprintf(err.Error())))
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(5).Infof(logString(fmt.Sprintf("deleted agreement %v from exchange", agreementId)))
			return nil
		}
	}

}

func (w *GovernanceWorker) deleteMessage(msg *exchange.DeviceMessage) error {
	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := w.Manager.Config.Edge.ExchangeURL + "orgs/" + exchange.GetOrg(w.deviceId) + "/nodes/" + exchange.GetId(w.deviceId) + "/msgs/" + strconv.Itoa(msg.MsgId)
	for {
		if err, tpErr := exchange.InvokeExchange(w.Config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), "DELETE", targetURL, w.deviceId, w.deviceToken, nil, &resp); err != nil {
			glog.Errorf(err.Error())
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(3).Infof(logString(fmt.Sprintf("deleted message %v", msg.MsgId)))
			return nil
		}
	}
}

func (w *GovernanceWorker) messageInExchange(msgId int) (bool, error) {
	var resp interface{}
	resp = new(exchange.GetDeviceMessageResponse)
	targetURL := w.Manager.Config.Edge.ExchangeURL + "orgs/" + exchange.GetOrg(w.deviceId) + "/nodes/" + exchange.GetId(w.deviceId) + "/msgs"
	for {
		if err, tpErr := exchange.InvokeExchange(w.Config.Collaborators.HTTPClientFactory.NewHTTPClient(nil), "GET", targetURL, w.deviceId, w.deviceToken, nil, &resp); err != nil {
			glog.Errorf(err.Error())
			return false, err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			msgs := resp.(*exchange.GetDeviceMessageResponse).Messages
			for _, msg := range msgs {
				if msg.MsgId == msgId {
					return true, nil
				}
			}
			return false, nil
		}
	}
}

var logString = func(v interface{}) string {
	return fmt.Sprintf("GovernanceWorker: %v", v)
}

// go through all the protocols and find the agreements with given agreement ids from the db
func (w *GovernanceWorker) FindEstablishedAgreementsWithIds(agreementIds []string) ([]persistence.EstablishedAgreement, error) {

	// filter for finding all
	multiIdFilter := func(ids []string) persistence.EAFilter {
		return func(e persistence.EstablishedAgreement) bool {
			for _, id := range ids {
				if e.CurrentAgreementId == id {
					return true
				}
			}
			return false
		}
	}

	if agreementIds == nil || len(agreementIds) == 0 {
		return make([]persistence.EstablishedAgreement, 0), nil
	}

	// find all agreements in db
	var filters []persistence.EAFilter
	filters = append(filters, persistence.UnarchivedEAFilter())
	filters = append(filters, multiIdFilter(agreementIds))
	return persistence.FindEstablishedAgreementsAllProtocols(w.db, policy.AllAgreementProtocols(), filters)
}

