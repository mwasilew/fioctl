package keys

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	canonical "github.com/docker/go/canonical/json"
	tuf "github.com/theupdateframework/notary/tuf/data"

	"github.com/foundriesio/fioctl/client"
	"github.com/foundriesio/fioctl/subcommands"
)

func init() {
	rotate := &cobra.Command{
		Use:   "rotate-offline-key --role root|targets --txid=<txid> --keys=<tuf-root-keys.tgz>",
		Short: "Stage rotation of the offline TUF signing key for the Factory",
		Long: `Stage rotation of the offline TUF signing key for the Factory.

The new offline signing key will be used in both CI and production TUF root.

When you rotate the TUF targets offline signing key:
- if there are production targets in your factory, they are re-signed using the new key.
- if there is an active wave in your factory, the TUF targets rotation is not allowed.`,
		Example: `
- Rotate offline TUF root key and re-sign the new TUF root with both old and new keys:
  fioctl keys tuf updates rotate-offline-key \
    --txid=abc --role=root --keys=tuf-root-keys.tgz --sign
- Rotate offline TUF root key explicitly specifying new key type (and signing algorithm):
  fioctl keys tuf updates rotate-offline-key \
    --txid=abc --role=root --keys=tuf-root-keys.tgz --key-type=ed25519
- Rotate offline TUF targets key and re-sign the new TUF root with offline TUF root key:
  fioctl keys tuf updates rotate-offline-key \
    --txid=abc --role=targets --keys=tuf-root-keys.tgz --sign
- Rotate offline TUF targets key and store the new key in a separate file (and re-sign TUF root):
  fioctl keys tuf updates rotate-offline-key \
    --txid=abc --role=targets --keys=tuf-root-keys.tgz --targets-keys=tuf-targets-keys.tgz --sign`,
		Run: doTufUpdatesRotateOfflineKey,
	}
	rotate.Flags().StringP("role", "r", "", "TUF role name, supported: Root, Targets.")
	_ = rotate.MarkFlagRequired("role")
	rotate.Flags().StringP("txid", "x", "", "TUF root updates transaction ID.")
	rotate.Flags().StringP("keys", "k", "", "Path to <tuf-root-keys.tgz> used to sign TUF root.")
	_ = rotate.MarkFlagFilename("keys")
	rotate.Flags().StringP("targets-keys", "K", "", "Path to <tuf-targets-keys.tgz> used to sign prod & wave TUF targets.")
	_ = rotate.MarkFlagFilename("targets-keys")
	rotate.Flags().StringP("key-type", "y", tufKeyTypeNameEd25519, "Key type, supported: Ed25519, RSA.")
	rotate.Flags().BoolP("sign", "s", false, "Sign the new TUF root using the offline root keys.")
	tufUpdatesCmd.AddCommand(rotate)
}

func doTufUpdatesRotateOfflineKey(cmd *cobra.Command, args []string) {
	roleName, _ := cmd.Flags().GetString("role")
	roleName = ParseTufRoleNameOffline(roleName)
	switch roleName {
	case tufRoleNameRoot:
		doTufUpdatesRotateOfflineRootKey(cmd)
	case tufRoleNameTargets:
		doTufUpdatesRotateOfflineTargetsKey(cmd)
	default:
		panic(fmt.Errorf("Unexpected role name: %s", roleName))
	}
}

func doTufUpdatesRotateOfflineRootKey(cmd *cobra.Command) {
	factory := viper.GetString("factory")
	txid, _ := cmd.Flags().GetString("txid")
	keyTypeStr, _ := cmd.Flags().GetString("key-type")
	keyType := ParseTufKeyType(keyTypeStr)
	keysFile, _ := cmd.Flags().GetString("keys")
	targetsKeysFile, _ := cmd.Flags().GetString("targets-keys")
	shouldSign, _ := cmd.Flags().GetBool("sign")

	if keysFile == "" {
		subcommands.DieNotNil(errors.New(
			"The --keys option is required to rotate the offline TUF root key.",
		))
	}
	if targetsKeysFile != "" {
		subcommands.DieNotNil(errors.New(
			"The --targets-keys option is only valid to rotate the offline TUF targets key.",
		))
	}

	creds, err := GetOfflineCreds(keysFile)
	subcommands.DieNotNil(err)
	subcommands.AssertWritable(keysFile)

	var updates client.TufRootUpdates
	updates, err = api.TufRootUpdatesGet(factory)
	subcommands.DieNotNil(err)

	curCiRoot, newCiRoot := checkTufRootUpdatesStatus(updates, true)

	// A rotation is pretty easy:
	// 1. change the who's listed as the root key
	// 2. sign the new root.json with both the old and new root
	newKey, newCreds := replaceOfflineRootKey(newCiRoot, creds, keyType)
	fmt.Println("= New root keyid:", newKey.Id)
	newCiRoot.Signatures = make([]tuf.Signature, 0)
	removeUnusedTufKeys(newCiRoot)
	newProdRoot := genProdTufRoot(newCiRoot)

	if shouldSign {
		signNewTufRoot(curCiRoot, newCiRoot, newProdRoot, newCreds)
	}

	fmt.Println("= Uploading new TUF root")
	tmpFile := saveTempTufCreds(keysFile, newCreds)
	err = api.TufRootUpdatesPut(factory, txid, newCiRoot, newProdRoot, nil)
	handleTufRootUpdatesUpload(tmpFile, keysFile, err)
}

func doTufUpdatesRotateOfflineTargetsKey(cmd *cobra.Command) {
	factory := viper.GetString("factory")
	txid, _ := cmd.Flags().GetString("txid")
	keyTypeStr, _ := cmd.Flags().GetString("key-type")
	keyType := ParseTufKeyType(keyTypeStr)
	keysFile, _ := cmd.Flags().GetString("keys")
	targetsKeysFile, _ := cmd.Flags().GetString("targets-keys")
	shouldSign, _ := cmd.Flags().GetBool("sign")

	if targetsKeysFile == "" {
		targetsKeysFile = keysFile
	}
	if targetsKeysFile == "" {
		subcommands.DieNotNil(errors.New(
			"The --keys or --targets-keys option is required to rotate the offline TUF targets key.",
		))
	}
	if shouldSign && keysFile == "" {
		subcommands.DieNotNil(errors.New("The --keys option is required to sign the new TUF root."))
	}

	var (
		creds, targetsCreds OfflineCreds
		err                 error
	)
	if _, err := os.Stat(targetsKeysFile); err == nil {
		targetsCreds, err = GetOfflineCreds(targetsKeysFile)
		subcommands.DieNotNil(err)
		subcommands.AssertWritable(targetsKeysFile)
	} else if errors.Is(err, fs.ErrNotExist) {
		targetsCreds = make(OfflineCreds, 0)
		saveTufCreds(targetsKeysFile, targetsCreds)
	} else {
		subcommands.DieNotNil(err)
	}

	if shouldSign {
		if keysFile == targetsKeysFile {
			creds = targetsCreds
		} else {
			creds, err = GetOfflineCreds(keysFile)
			subcommands.DieNotNil(err)
		}
	}

	updates, err := api.TufRootUpdatesGet(factory)
	subcommands.DieNotNil(err)

	curCiRoot, newCiRoot := checkTufRootUpdatesStatus(updates, true)

	// Target "rotation" works like this:
	// 1. Find the "online target key" - this the key used by CI, so we don't
	//    want to lose it.
	// 2. Generate a new key.
	// 3. Set these keys in root.json.
	// 4. Re-sign existing production targets.
	onlineTargetsId := updates.Updated.OnlineKeys["targets"]
	if onlineTargetsId == "" {
		subcommands.DieNotNil(errors.New("Unable to find online target key for factory"))
	}
	subcommands.DieNotNil(err)
	newKey, newCreds := replaceOfflineTargetsKey(newCiRoot, onlineTargetsId, targetsCreds, keyType)
	fmt.Println("= New target keyid:", newKey.Id)
	newCiRoot.Signatures = make([]tuf.Signature, 0)
	removeUnusedTufKeys(newCiRoot)
	newProdRoot := genProdTufRoot(newCiRoot)

	fmt.Println("= Re-signing prod targets")
	newTargetsSigs, err := resignProdTargets(factory, newCiRoot, onlineTargetsId, newCreds)
	subcommands.DieNotNil(err)

	if shouldSign {
		signNewTufRoot(curCiRoot, newCiRoot, newProdRoot, creds)
	}

	fmt.Println("= Uploading new TUF root")
	tmpFile := saveTempTufCreds(targetsKeysFile, newCreds)
	err = api.TufRootUpdatesPut(factory, txid, newCiRoot, newProdRoot, newTargetsSigs)
	handleTufRootUpdatesUpload(tmpFile, targetsKeysFile, err)
}

func replaceOfflineRootKey(
	root *client.AtsTufRoot, creds OfflineCreds, keyType TufKeyType,
) (*TufSigner, OfflineCreds) {
	kp := genTufKeyPair(keyType)
	root.Signed.Keys[kp.signer.Id] = kp.atsPub
	root.Signed.Expires = time.Now().AddDate(1, 0, 0).UTC().Round(time.Second) // 1 year validity
	root.Signed.Roles["root"].KeyIDs = []string{kp.signer.Id}

	base := "tufrepo/keys/fioctl-root-" + kp.signer.Id
	creds[base+".pub"] = kp.atsPubBytes
	creds[base+".sec"] = kp.atsPrivBytes
	return &kp.signer, creds
}

func replaceOfflineTargetsKey(
	root *client.AtsTufRoot, onlineTargetsId string, creds OfflineCreds, keyType TufKeyType,
) (*TufSigner, OfflineCreds) {
	kp := genTufKeyPair(keyType)
	root.Signed.Keys[kp.signer.Id] = kp.atsPub
	root.Signed.Roles["targets"].KeyIDs = []string{onlineTargetsId, kp.signer.Id}
	root.Signed.Roles["targets"].Threshold = 1

	base := "tufrepo/keys/fioctl-targets-" + kp.signer.Id
	creds[base+".pub"] = kp.atsPubBytes
	creds[base+".sec"] = kp.atsPrivBytes
	return &kp.signer, creds
}

func resignProdTargets(
	factory string, root *client.AtsTufRoot, onlineTargetsId string, creds OfflineCreds,
) (map[string][]tuf.Signature, error) {
	targetsMap, err := api.ProdTargetsList(factory, false)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch production targets: %w", err)
	} else if targetsMap == nil {
		return nil, nil
	}

	var signers []TufSigner
	for _, kid := range root.Signed.Roles["targets"].KeyIDs {
		if kid == onlineTargetsId {
			continue
		}
		pub := root.Signed.Keys[kid].KeyValue.Public
		signer, err := FindTufSigner(kid, pub, creds)
		if err != nil {
			return nil, fmt.Errorf("Failed to find private key for %s: %w", kid, err)
		}
		signers = append(signers, *signer)
	}

	signatureMap := make(map[string][]tuf.Signature)
	for tag, targets := range targetsMap {
		bytes, err := canonical.MarshalCanonical(targets.Signed)
		if err != nil {
			return nil, fmt.Errorf("Failed to marshal targets for tag %s: %w", tag, err)
		}
		signatures, err := SignTufMeta(bytes, signers...)
		if err != nil {
			return nil, fmt.Errorf("Failed to re-sign targets for tag %s: %w", tag, err)
		}
		signatureMap[tag] = signatures
	}
	return signatureMap, nil
}

func handleTufRootUpdatesUpload(tmpKeysFile, keysFile string, err error) {
	if err != nil {
		if omg := os.Remove(tmpKeysFile); omg != nil {
			fmt.Printf("Failed to remove a temporary keys file %s: %v.\n", tmpKeysFile, omg)
		}
		subcommands.DieNotNil(err)
	}
	if err = os.Rename(tmpKeysFile, keysFile); err != nil {
		fmt.Println("\nERROR: Unable to update offline keys file.", err)
		fmt.Println("Temp copy still available at:", tmpKeysFile)
		fmt.Println("This temp file contains your new factory private key. You must copy this file.")
	}
}
