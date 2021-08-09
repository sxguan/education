
package sdkInit

import (
	"fmt"
	mb "github.com/hyperledger/fabric-protos-go/msp"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/channel"
	mspclient "github.com/hyperledger/fabric-sdk-go/pkg/client/msp"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/resmgmt"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/retry"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/status"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/msp"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config"
	lcpackager "github.com/hyperledger/fabric-sdk-go/pkg/fab/ccpackager/lifecycle"
	"github.com/hyperledger/fabric-sdk-go/pkg/fabsdk"
	"github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/common/policydsl"
	"strings"
)

func Setup(configFile string, info *SdkEnvInfo) (*fabsdk.FabricSDK, error) {
	// Create SDK setup for the integration tests
	var err error
	sdk, err := fabsdk.New(config.FromFile(configFile))
	if err != nil {
		return nil, err
	}

	// 为组织获得Client句柄和Context信息
	for _, org := range info.Orgs {
		org.orgMspClient, err = mspclient.New(sdk.Context(), mspclient.WithOrg(org.OrgName))
		if err != nil {
			return nil, err
		}
		orgContext := sdk.Context(fabsdk.WithUser(org.OrgAdminUser), fabsdk.WithOrg(org.OrgName))
		org.OrgAdminClientContext = &orgContext

		// New returns a resource management client instance.
		resMgmtClient, err := resmgmt.New(orgContext)
		if err != nil {
			return nil, fmt.Errorf("根据指定的资源管理客户端Context创建通道管理客户端失败: %v", err)
		}
		org.OrgResMgmt = resMgmtClient
	}

	// 为Orderer获得Context信息
	ordererClientContext := sdk.Context(fabsdk.WithUser(info.OrdererAdminUser), fabsdk.WithOrg(info.OrdererOrgName))
	info.OrdererClientContext = &ordererClientContext
	return sdk, nil
}

func CreateAndJoinChannel(info *SdkEnvInfo) error {
	fmt.Println(">> 开始创建通道......")
	if len(info.Orgs) == 0 {
		return fmt.Errorf("通道组织不能为空，请提供组织信息")
	}

	// 获得所有组织的签名信息
	signIds := []msp.SigningIdentity{}
	for _, org := range info.Orgs {
		// Get signing identity that is used to sign create channel request
		orgSignId, err := org.orgMspClient.GetSigningIdentity(org.OrgAdminUser)
		if err != nil {
			return fmt.Errorf("GetSigningIdentity error: %v", err)
		}
		signIds = append(signIds, orgSignId)
	}

	// 创建通道
	if err := createChannel(signIds, info); err != nil {
		return fmt.Errorf("Create channel error: %v", err)
	}

	fmt.Println(">> 创建通道成功")

	fmt.Println(">> 加入通道......")
	for _, org := range info.Orgs {
		// 加入通道
		// Org peers join channel
		if err := org.OrgResMgmt.JoinChannel(info.ChannelID, resmgmt.WithRetry(retry.DefaultResMgmtOpts), resmgmt.WithOrdererEndpoint("orderer.example.com")); err != nil {
			return fmt.Errorf("%s peers failed to JoinChannel: %v", org.OrgName, err)
		}
	}
	fmt.Println(">> 加入通道成功")
	return nil
}

func createChannel(signIDs []msp.SigningIdentity, info *SdkEnvInfo) error {
	// Channel management client is responsible for managing channels (create/update channel)
	chMgmtClient, err := resmgmt.New(*info.OrdererClientContext)
	if err != nil {
		return fmt.Errorf("Channel management client create error: %v", err)
	}

	// create a channel for orgchannel.tx
	req := resmgmt.SaveChannelRequest{ChannelID: info.ChannelID,
		ChannelConfigPath: info.ChannelConfig,
		SigningIdentities: signIDs}

	if _, err := chMgmtClient.SaveChannel(req, resmgmt.WithRetry(retry.DefaultResMgmtOpts), resmgmt.WithOrdererEndpoint("orderer.example.com")); err != nil {
		return fmt.Errorf("error should be nil for SaveChannel of orgchannel: %v", err)
	}

	fmt.Println(">>>> 使用每个org的管理员身份更新锚节点配置...")
	//do the same get ch client and create channel for each anchor peer as well (first for Org1MSP)
	for i, org := range info.Orgs {
		req = resmgmt.SaveChannelRequest{ChannelID: info.ChannelID,
			ChannelConfigPath: org.OrgAnchorFile,
			SigningIdentities: []msp.SigningIdentity{signIDs[i]}}

		if _, err = org.OrgResMgmt.SaveChannel(req, resmgmt.WithRetry(retry.DefaultResMgmtOpts), resmgmt.WithOrdererEndpoint("orderer.example.com")); err != nil {
			return fmt.Errorf("SaveChannel for anchor org %s error: %v", org.OrgName, err)
		}
	}
	fmt.Println(">>>> 使用每个org的管理员身份更新锚节点配置完成")
	//integration.WaitForOrdererConfigUpdate(t, configQueryClient, mc.channelID, false, lastConfigBlock)
	return nil
}

func CreateCCLifecycle(info *SdkEnvInfo, sequence int64, upgrade bool, sdk *fabsdk.FabricSDK) error {
	if len(info.Orgs) == 0 {
		return fmt.Errorf("the number of organization should not be zero.")
	}
	// Package cc
	fmt.Println(">> 开始打包链码......")
	label, ccPkg, err := packageCC(info.ChaincodeID, info.ChaincodeVersion, info.ChaincodePath)
	if err != nil {
		return fmt.Errorf("pakcagecc error: %v", err)
	}
	packageID := lcpackager.ComputePackageID(label, ccPkg)
	fmt.Println(">> 打包链码成功")

	// Install cc
	fmt.Println(">> 开始安装链码......")
	if err := installCC(label, ccPkg, info.Orgs); err != nil {
		return fmt.Errorf("installCC error: %v", err)
	}

	// Get installed cc package
	if err := getInstalledCCPackage(packageID, info.Orgs[0]); err != nil {
		return fmt.Errorf("getInstalledCCPackage error: %v", err)
	}

	// Query installed cc
	if err := queryInstalled(packageID, info.Orgs[0]); err != nil {
		return fmt.Errorf("queryInstalled error: %v", err)
	}
	fmt.Println(">> 安装链码成功")

	// Approve cc
	fmt.Println(">> 组织认可智能合约定义......")
	if err := approveCC(packageID, info.ChaincodeID, info.ChaincodeVersion, sequence, info.ChannelID, info.Orgs, info.OrdererEndpoint); err != nil {
		return fmt.Errorf("approveCC error: %v", err)
	}

	// Query approve cc
	if err:=queryApprovedCC(info.ChaincodeID, sequence, info.ChannelID, info.Orgs);err!=nil{
		return fmt.Errorf("queryApprovedCC error: %v", err)
	}
	fmt.Println(">> 组织认可智能合约定义完成")

	// Check commit readiness
	fmt.Println(">> 检查智能合约是否就绪......")
	if err:=checkCCCommitReadiness(packageID, info.ChaincodeID, info.ChaincodeVersion, sequence, info.ChannelID, info.Orgs); err!=nil{
		return fmt.Errorf("checkCCCommitReadiness error: %v", err)
	}
	fmt.Println(">> 智能合约已经就绪")

	// Commit cc
	fmt.Println(">> 提交智能合约定义......")
	if err:=commitCC(info.ChaincodeID, info.ChaincodeVersion, sequence, info.ChannelID, info.Orgs, info.OrdererEndpoint);err!=nil{
		return fmt.Errorf("commitCC error: %v", err)
	}
	// Query committed cc
	if err:=queryCommittedCC(info.ChaincodeID, info.ChannelID, sequence, info.Orgs); err!=nil{
		return fmt.Errorf("queryCommittedCC error: %v", err)
	}
	fmt.Println(">> 智能合约定义提交完成")

	// Init cc
	fmt.Println(">> 调用智能合约初始化方法......")
	if err:=initCC(info.ChaincodeID, upgrade, info.ChannelID, info.Orgs[0], sdk); err!=nil{
		return fmt.Errorf("initCC error: %v", err)
	}
	fmt.Println(">> 完成智能合约初始化")
	return nil
}

func packageCC(ccName, ccVersion, ccpath string) (string, []byte, error) {
	label := ccName + "_" + ccVersion
	desc := &lcpackager.Descriptor{
		Path:  ccpath,
		Type:  pb.ChaincodeSpec_GOLANG,
		Label: label,
	}
	ccPkg, err := lcpackager.NewCCPackage(desc)
	if err != nil {
		return "", nil, fmt.Errorf("Package chaincode source error: %v", err)
	}
	return desc.Label, ccPkg, nil
}

func installCC(label string, ccPkg []byte, orgs []*OrgInfo) error {
	installCCReq := resmgmt.LifecycleInstallCCRequest{
		Label:   label,
		Package: ccPkg,
	}

	packageID := lcpackager.ComputePackageID(installCCReq.Label, installCCReq.Package)
	for _, org := range orgs {
		orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, org.OrgPeerNum)
		if err != nil {
			fmt.Errorf("DiscoverLocalPeers error: %v", err)
		}
		if flag, _ := checkInstalled(packageID, orgPeers[0], org.OrgResMgmt); flag == false {
			if _, err := org.OrgResMgmt.LifecycleInstallCC(installCCReq, resmgmt.WithTargets(orgPeers...), resmgmt.WithRetry(retry.DefaultResMgmtOpts)); err != nil {
				return fmt.Errorf("LifecycleInstallCC error: %v", err)
			}
		}
	}
	return nil
}

func getInstalledCCPackage(packageID string, org *OrgInfo) error {
	// use org1
	orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, 1)
	if err != nil {
		return fmt.Errorf("DiscoverLocalPeers error: %v", err)
	}

	if _, err := org.OrgResMgmt.LifecycleGetInstalledCCPackage(packageID, resmgmt.WithTargets([]fab.Peer{orgPeers[0]}...)); err != nil {
		return fmt.Errorf("LifecycleGetInstalledCCPackage error: %v", err)
	}
	return nil
}

func queryInstalled(packageID string, org *OrgInfo) error {
	orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, 1)
	if err != nil {
		return fmt.Errorf("DiscoverLocalPeers error: %v", err)
	}
	resp1, err := org.OrgResMgmt.LifecycleQueryInstalledCC(resmgmt.WithTargets([]fab.Peer{orgPeers[0]}...))
	if err != nil {
		return fmt.Errorf("LifecycleQueryInstalledCC error: %v", err)
	}
	packageID1 := ""
	for _, t := range resp1 {
		if t.PackageID == packageID {
			packageID1 = t.PackageID
		}
	}
	if !strings.EqualFold(packageID, packageID1) {
		return fmt.Errorf("check package id error")
	}
	return nil
}

func checkInstalled(packageID string, peer fab.Peer, client *resmgmt.Client) (bool, error) {
	flag := false
	resp1, err := client.LifecycleQueryInstalledCC(resmgmt.WithTargets(peer))
	if err != nil {
		return flag, fmt.Errorf("LifecycleQueryInstalledCC error: %v", err)
	}
	for _, t := range resp1 {
		if t.PackageID == packageID {
			flag = true
		}
	}
	return flag, nil
}

func approveCC(packageID string, ccName, ccVersion string, sequence int64, channelID string, orgs []*OrgInfo, ordererEndpoint string) error {
	mspIDs := []string{}
	for _, org := range orgs {
		mspIDs = append(mspIDs, org.OrgMspId)
	}
	ccPolicy := policydsl.SignedByNOutOfGivenRole(int32(len(mspIDs)), mb.MSPRole_MEMBER, mspIDs)
	approveCCReq := resmgmt.LifecycleApproveCCRequest{
		Name:              ccName,
		Version:           ccVersion,
		PackageID:         packageID,
		Sequence:          sequence,
		EndorsementPlugin: "escc",
		ValidationPlugin:  "vscc",
		SignaturePolicy:   ccPolicy,
		InitRequired:      true,
	}

	for _, org := range orgs{
		orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, org.OrgPeerNum)
		fmt.Printf(">>> chaincode approved by %s peers:\n", org.OrgName)
		for _, p := range orgPeers {
			fmt.Printf("	%s\n", p.URL())
		}

		if err!=nil{
			return fmt.Errorf("DiscoverLocalPeers error: %v", err)
		}
		if _, err := org.OrgResMgmt.LifecycleApproveCC(channelID, approveCCReq, resmgmt.WithTargets(orgPeers...), resmgmt.WithOrdererEndpoint(ordererEndpoint), resmgmt.WithRetry(retry.DefaultResMgmtOpts));err != nil {
			fmt.Errorf("LifecycleApproveCC error: %v", err)
		}
	}
	return nil
}

func queryApprovedCC(ccName string, sequence int64, channelID string, orgs []*OrgInfo) error {
	queryApprovedCCReq := resmgmt.LifecycleQueryApprovedCCRequest{
		Name:     ccName,
		Sequence: sequence,
	}

	for _, org := range orgs{
		orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, org.OrgPeerNum)
		if err!=nil{
			return fmt.Errorf("DiscoverLocalPeers error: %v", err)
		}
		// Query approve cc
		for _, p := range orgPeers {
			resp, err := retry.NewInvoker(retry.New(retry.TestRetryOpts)).Invoke(
				func() (interface{}, error) {
					resp1, err := org.OrgResMgmt.LifecycleQueryApprovedCC(channelID, queryApprovedCCReq, resmgmt.WithTargets(p))
					if err != nil {
						return nil, status.New(status.TestStatus, status.GenericTransient.ToInt32(), fmt.Sprintf("LifecycleQueryApprovedCC returned error: %v", err), nil)
					}
					return resp1, err
				},
			)
			if err != nil {
				return fmt.Errorf("Org %s Peer %s NewInvoker error: %v", org.OrgName, p.URL(), err)
			}
			if resp==nil{
				return fmt.Errorf("Org %s Peer %s Got nil invoker", org.OrgName, p.URL())
			}
		}
	}
	return nil
}

func checkCCCommitReadiness(packageID string, ccName, ccVersion string, sequence int64, channelID string, orgs []*OrgInfo) error {
	mspIds := []string{}
	for _, org := range orgs {
		mspIds = append(mspIds, org.OrgMspId)
	}
	ccPolicy := policydsl.SignedByNOutOfGivenRole(int32(len(mspIds)), mb.MSPRole_MEMBER, mspIds)
	req := resmgmt.LifecycleCheckCCCommitReadinessRequest{
		Name:              ccName,
		Version:           ccVersion,
		//PackageID:         packageID,
		EndorsementPlugin: "escc",
		ValidationPlugin:  "vscc",
		SignaturePolicy:   ccPolicy,
		Sequence:          sequence,
		InitRequired:      true,
	}
	for _, org := range orgs{
		orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, org.OrgPeerNum)
		if err!=nil{
			fmt.Errorf("DiscoverLocalPeers error: %v", err)
		}
		for _, p := range orgPeers {
			resp, err := retry.NewInvoker(retry.New(retry.TestRetryOpts)).Invoke(
				func() (interface{}, error) {
					resp1, err := org.OrgResMgmt.LifecycleCheckCCCommitReadiness(channelID, req, resmgmt.WithTargets(p))
					fmt.Printf("LifecycleCheckCCCommitReadiness cc = %v, = %v\n", ccName, resp1)
					if err != nil {
						return nil, status.New(status.TestStatus, status.GenericTransient.ToInt32(), fmt.Sprintf("LifecycleCheckCCCommitReadiness returned error: %v", err), nil)
					}
					flag := true
					for _, r := range resp1.Approvals {
						flag = flag && r
					}
					if !flag {
						return nil, status.New(status.TestStatus, status.GenericTransient.ToInt32(), fmt.Sprintf("LifecycleCheckCCCommitReadiness returned : %v", resp1), nil)
					}
					return resp1, err
				},
			)
			if err != nil {
				return fmt.Errorf("NewInvoker error: %v", err)
			}
			if resp==nil{
				return fmt.Errorf("Got nill invoker response")
			}
		}
	}

	return nil
}

func commitCC(ccName, ccVersion string, sequence int64, channelID string, orgs []*OrgInfo, ordererEndpoint string) error{
	mspIDs := []string{}
	for _, org := range orgs {
		mspIDs = append(mspIDs, org.OrgMspId)
	}
	ccPolicy := policydsl.SignedByNOutOfGivenRole(int32(len(mspIDs)), mb.MSPRole_MEMBER, mspIDs)

	req := resmgmt.LifecycleCommitCCRequest{
		Name:              ccName,
		Version:           ccVersion,
		Sequence:          sequence,
		EndorsementPlugin: "escc",
		ValidationPlugin:  "vscc",
		SignaturePolicy:   ccPolicy,
		InitRequired:      true,
	}
	_, err := orgs[0].OrgResMgmt.LifecycleCommitCC(channelID, req, resmgmt.WithOrdererEndpoint(ordererEndpoint), resmgmt.WithRetry(retry.DefaultResMgmtOpts))
	if err != nil {
		return fmt.Errorf("LifecycleCommitCC error: %v", err)
	}
	return nil
}

func queryCommittedCC( ccName string, channelID string, sequence int64, orgs []*OrgInfo) error {
	req := resmgmt.LifecycleQueryCommittedCCRequest{
		Name: ccName,
	}

	for _, org := range orgs {
		orgPeers, err := DiscoverLocalPeers(*org.OrgAdminClientContext, org.OrgPeerNum)
		if err!=nil{
			return fmt.Errorf("DiscoverLocalPeers error: %v", err)
		}
		for _, p := range orgPeers {
			resp, err := retry.NewInvoker(retry.New(retry.TestRetryOpts)).Invoke(
				func() (interface{}, error) {
					resp1, err := org.OrgResMgmt.LifecycleQueryCommittedCC(channelID, req, resmgmt.WithTargets(p))
					if err != nil {
						return nil, status.New(status.TestStatus, status.GenericTransient.ToInt32(), fmt.Sprintf("LifecycleQueryCommittedCC returned error: %v", err), nil)
					}
					flag := false
					for _, r := range resp1 {
						if r.Name == ccName && r.Sequence == sequence {
							flag = true
							break
						}
					}
					if !flag {
						return nil, status.New(status.TestStatus, status.GenericTransient.ToInt32(), fmt.Sprintf("LifecycleQueryCommittedCC returned : %v", resp1), nil)
					}
					return resp1, err
				},
			)
			if err != nil {
				return  fmt.Errorf("NewInvoker error: %v", err)
			}
			if resp==nil{
				return fmt.Errorf("Got nil invoker response")
			}
		}
	}
	return nil
}

func initCC(ccName string, upgrade bool, channelID string, org *OrgInfo, sdk *fabsdk.FabricSDK) error {
	//prepare channel client context using client context
	clientChannelContext := sdk.ChannelContext(channelID, fabsdk.WithUser(org.OrgUser), fabsdk.WithOrg(org.OrgName))
	// Channel client is used to query and execute transactions (Org1 is default org)
	client, err := channel.New(clientChannelContext)
	if err != nil {
		return fmt.Errorf("Failed to create new channel client: %s", err)
	}

	// init
	_, err = client.Execute(channel.Request{ChaincodeID: ccName, Fcn: "init", Args: nil, IsInit: true},
		channel.WithRetry(retry.DefaultChannelOpts))
	if err != nil {
		return fmt.Errorf("Failed to init: %s", err)
	}
	return nil
}
