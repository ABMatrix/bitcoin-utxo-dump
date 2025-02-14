package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"go.mongodb.org/mongo-driver/mongo/writeconcern"

	"github.com/ABMatrix/bitcoin-utxo/bitcoin/bech32"
	"github.com/ABMatrix/bitcoin-utxo/bitcoin/btcleveldb"
	"github.com/ABMatrix/bitcoin-utxo/bitcoin/keys"

	"github.com/btcsuite/btcd/txscript"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Version
const (
	Version  = "0.1.2"
	MAX_JOBS = 8
	// TODO make the following cmd options
	ENV_MONGO_URI               = "MONGO_URI"
	ENV_MONGO_BITCOIN_DB_NAME   = "MONGO_UTXO_DB_NAME"
	UTXO_COLLECTION_NAME_PREFIX = "utxo"
	BATCH_SIZE                  = 1 << 13
	PATH_FOR_FAILED_PREFIX      = "/root/bitcoin-failed"
)

var (
	mongoCli       *mongo.Client
	utxoCollection *mongo.Collection
	maxJobs        int
	pathForFailed  string
)

func main() {
	// Set default chainstate LevelDB and output file
	defaultFolder := fmt.Sprintf("%s/.bitcoin/chainstate/", os.Getenv("HOME")) // %s = string

	// Command Line Options (Flags)
	chainstate := flag.String("db", defaultFolder, "Location of bitcoin chainstate db.") // chainstate folder
	testnetFlag := flag.Bool("testnet", false, "Is the chainstate leveldb for testnet?") // true/false
	version := flag.Bool("version", false, "Print version.")
	jobs := flag.Int("max-jobs", MAX_JOBS, "the maximum concurrent jobs")
	failedOnly := flag.Bool("failed", false, "process failed only")
	flag.Parse() // execute command line parsing for all declared flags

	// Show Version
	if *version {
		log.Println(Version)
		os.Exit(0)
	}

	maxJobs = *jobs

	ctx := context.Background()

	// Mainnet or Testnet (for encoding addresses correctly)
	testnet := false
	utxoCollectionName := UTXO_COLLECTION_NAME_PREFIX + "-mainnet"
	pathForFailed = PATH_FOR_FAILED_PREFIX + "-mainnet"
	if *testnetFlag || strings.Contains(*chainstate, "testnet") { // check testnet flag
		testnet = true
		utxoCollectionName = UTXO_COLLECTION_NAME_PREFIX + "-testnet"
		pathForFailed = PATH_FOR_FAILED_PREFIX + "-testnet"
	}

	if _, err := os.Stat(pathForFailed); os.IsNotExist(err) {
		err = os.Mkdir(pathForFailed, os.ModePerm)
		if err != nil {
			log.Fatalln("[fatal] failed to create", pathForFailed, "with error:", err.Error())
		}
	}

	// Select bitcoin chainstate leveldb folder
	// open leveldb without compression to avoid corrupting the database for bitcoin
	opts := &opt.Options{
		ErrorIfMissing: true,
		Compression:    opt.NoCompression,
		ReadOnly:       true,
	}
	// https://bitcoin.stackexchange.com/questions/52257/chainstate-leveldb-corruption-after-reading-from-the-database
	// https://github.com/syndtr/goleveldb/issues/61
	// https://godoc.org/github.com/syndtr/goleveldb/leveldb/opt

	db, err := leveldb.OpenFile(*chainstate, opts) // You have got to dereference the pointer to get the actual value
	if err != nil {
		log.Println("[error] failed to open LevelDB with error: ", err.Error())
		return
	}
	defer db.Close()

	// Stats - keep track of interesting stats as we read through leveldb.
	var totalAmount int64 = 0 // total amount of satoshis
	scriptTypeCount := map[string]int64{
		NONSTANDARD:           0,
		PUBKEY:                0,
		PUBKEYHASH:            0,
		SCRIPTHASH:            0,
		MULTISIG:              0,
		WITNESS_V0_KEYHASH:    0,
		WITNESS_V0_SCRIPTHASH: 0,
		WITNESS_V1_TAPROOT:    0,
		WITNESS_UNKNOWN:       0,
		NULLDATA:              0,
	} // count each script type

	// Iterate over LevelDB keys
	iter := db.NewIterator(nil, nil)
	// NOTE: iter.Release() comes after the iteration (not deferred here)
	if err := iter.Error(); err != nil {
		fmt.Println("[fatal] failed to iterate over level DB keys", err)
		os.Exit(-1)
	}

	// MongoDB version of API service
	mongoURI := os.Getenv(ENV_MONGO_URI) // "mongodb://username@password:<ip>:port/"
	if mongoURI == "" {
		log.Fatalln("[fatal] mongo URI is unset")
	}

	mongoDBName := os.Getenv(ENV_MONGO_BITCOIN_DB_NAME) // "bitcoin"
	if mongoDBName == "" {
		log.Fatalln("[fatal] mongo db name is unset")
	}

	// initialize mongodb
	clientOptions := options.Client().ApplyURI(mongoURI)

	// connect to MongoDB
	mongoCli, err = mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatalln("[fatal] failed to connect with error:", err)
	}
	defer mongoCli.Disconnect(ctx)
	// check connection
	err = mongoCli.Ping(ctx, nil)
	if err != nil {
		log.Fatalln("[fatal] failed to ping mongodb with error: ", err)
	}

	log.Println("[info] mongo connection is OK...")
	wcMajority := writeconcern.New(writeconcern.WMajority())
	wcMajorityCollectionOpts := options.Collection().SetWriteConcern(wcMajority)
	utxoCollection = mongoCli.Database(mongoDBName).Collection(utxoCollectionName, wcMajorityCollectionOpts)

	// Catch signals that interrupt the script so that we can close the database safely (hopefully not corrupting it)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() { // goroutine
		<-c // receive from channel
		log.Println("Interrupt signal caught. Shutting down gracefully.")
		iter.Release() // release database iterator
		db.Close()     // close database
		mongoCli.Disconnect(ctx)
		os.Exit(0) // exit
	}()

	log.Println("[info] start processing all failed...")
	processFailed(ctx)

	if *failedOnly {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Second)
	}

	// get obfuscate key (a byte slice)
	if ok := iter.Seek([]byte{14}); !ok {
		log.Println("[fatal] cannot find any key with prefix 14")
		iter.Release()
		db.Close()
		os.Exit(-1)
	}
	obfuscateKey := iter.Value()
	log.Println("[info] obfuscate key: ", obfuscateKey)

	var count int64
	var utxoBuf []*UTXO
	docsChan := make(chan []interface{}, maxJobs)
	totalJobs := 0
	for ok := iter.Seek([]byte{0x43}); ok; ok = iter.Next() {
		totalJobs++
	}
	totalJobs = int(math.Ceil(float64(totalJobs) / float64(BATCH_SIZE)))
	log.Println("[info] finished pre-scan of leveldb; total jobs:", totalJobs)
	resultsChan := make(chan int, totalJobs)
	for workerID := 1; workerID <= maxJobs; workerID++ {
		go worker(ctx, workerID, docsChan, resultsChan)
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		finished := 0
		for i := 0; i < totalJobs; i++ {
			finished += <-resultsChan
			log.Printf("[info] finished %d/%d\n", finished, totalJobs)
		}
	}()
	var entries int64
	for ok := iter.Seek([]byte{0x43}); ok; ok = iter.Next() {
		if entries > 0 && entries%BATCH_SIZE == 0 {
			// convert to mongo-acceptable arguments...
			var docs []interface{}
			for _, utxo := range utxoBuf {
				docs = append(docs, utxo)
			}
			utxoBuf = make([]*UTXO, 0)
			docsChan <- docs
		}
		entries++

		key, value := make([]byte, len(iter.Key())), make([]byte, len(iter.Value()))
		copy(key, iter.Key())
		copy(value, iter.Value())

		utxo, err := processEachEntry(key, value, obfuscateKey, testnet)
		if err != nil {
			log.Printf("[error] failed to process (%+v, %+v) with error: %s\n", key, value, err.Error())
			continue
		}
		scriptTypeCount[utxo.Type]++
		if utxo.Type == NULLDATA {
			// we don't want to insert unspendable coins
			continue
		}
		totalAmount += utxo.Amount
		utxoBuf = append(utxoBuf, utxo)
		count++
	}
	iter.Release() // Do not defer this, want to release iterator before closing database

	{
		// convert to mongo-acceptable arguments...
		var docs []interface{}
		for _, utxo := range utxoBuf {
			docs = append(docs, utxo)
		}
		utxoBuf = make([]*UTXO, 0)
		docsChan <- docs
	}

	wg.Wait()
	close(docsChan)
	close(resultsChan)

	processFailed(ctx)

	// Final Report
	// ---------------------
	log.Printf("[summary] Total entries in leveldb: %d\n", entries)
	log.Printf("[summary] Total spendable UTXOs: %d\n", count)

	// Can only show total btc amount if we have requested to get the amount for each entry with the -f fields flag
	log.Printf("[summary] Total BTC:   %.8f\n", float64(totalAmount)/1e8) // convert satoshis to BTC (float with 8 decimal places)

	// Can only show script type stats if we have requested to get the script type for each entry with the -f fields flag
	log.Println("[summary] Script Types:")
	for k, v := range scriptTypeCount {
		log.Printf(" %-12s %d\n", k, v) // %-12s = left-justify padding
	}
}

func worker(ctx context.Context, id int, docsChan chan []interface{}, results chan<- int) {
	for docs := range docsChan {
		if len(docs) <= 0 {
			fmt.Println("[info] worker", id, "finished with nothing to do")
			results <- 1
			continue
		}
		err := insertUTXOToMongo(ctx, docs)
		if err != nil {
			log.Printf("[error] worker %d failed to insert many with error: %s\n", id, err.Error())
			// write to a json file
			filename := fmt.Sprintf("%d-%s.json", id, docs[0].(*UTXO).ID)
			filepath := path.Join(pathForFailed, filename)
			file, err := os.Create(filepath)
			if err != nil {
				log.Println("[fatal] failed to create", filepath, "with error:", err.Error(), " now quitting")
				syscall.Kill(os.Getpid(), syscall.SIGTERM)
				return
			}
			log.Println("[info] start to write uninserted documents to", filepath)
			err = json.NewEncoder(file).Encode(docs)
			if err != nil {
				file.Close()
				log.Println("[fatal] failed to encode with error:", err.Error(), " now quitting")
				syscall.Kill(os.Getpid(), syscall.SIGTERM)
				return
			}
			file.Close()
		}
		results <- 1
	}
}

func processEachEntry(key []byte, value []byte, obfuscateKey []byte, testnet bool) (*UTXO, error) {
	// Output Fields - build output from flags passed in
	output := &UTXO{} // we will add to this as we go through each utxo in the database

	// ---
	// Key
	// ---

	//      430000155b9869d56c66d9e86e3c01de38e3892a42b99949fe109ac034fff6583900
	//      <><--------------------------------------------------------------><>
	//      /                               |                                  \
	//  type                          txid (little-endian)                      vout (varint)

	// txid
	txid := key[1:33] // little-endian byte order
	lenTxid := len(txid)

	// txid - reverse byte order
	for i := 0; i < lenTxid/2; i++ { // run backwards through the txid slice
		txid[i], txid[lenTxid-1-i] = txid[lenTxid-1-i], txid[i] // append each byte to the new byte slice
	}
	output.TxID = hex.EncodeToString(txid) // add to output results map

	// vout
	vout := key[33:]

	// convert varint128 vout to an integer
	output.Vout = btcleveldb.Varint128Decode(vout)

	uniqueKey := fmt.Sprintf("%s-%d", output.TxID, output.Vout)
	output.ID = uniqueKey

	// -----
	// Value
	// -----

	// Only deobfuscate and get data from the Value if something is needed from it (improves speed if you just want the txid:vout)

	// Copy the obfuscateKey ready to extend it
	obfuscateKeyExtended := obfuscateKey[1:] // ignore the first byte, as that just tells you the size of the obfuscateKey

	// Extend the obfuscateKey so it's the same length as the value
	for k := 0; len(obfuscateKeyExtended) < len(value); k++ {
		// append each byte of obfuscateKey to the end until it's the same length as the value
		obfuscateKeyExtended = append(obfuscateKeyExtended, obfuscateKeyExtended[k])
		// Example
		//   [8 175 184 95 99 240 37 253 115 181 161 4 33 81 167 111 145 131 0 233 37 232 118 180 123 120 78]
		//   [8 177 45 206 253 143 135 37 54]                                                                  <- obfuscate key
		//   [8 177 45 206 253 143 135 37 54 8 177 45 206 253 143 135 37 54 8 177 45 206 253 143 135 37 54]    <- extended
	}

	// XOR the value with the obfuscateKey (xor each byte) to de-obfuscate the value
	var xor []byte // create a byte slice to hold the xor results
	for i, b := range value {
		xor = append(xor, b^obfuscateKeyExtended[i])
	}

	// -----
	// Value
	// -----

	//   value: 71a9e87d62de25953e189f706bcf59263f15de1bf6c893bda9b045 <- obfuscated
	//          b12dcefd8f872536b12dcefd8f872536b12dcefd8f872536b12dce <- extended obfuscateKey (XOR)
	//          c0842680ed5900a38f35518de4487c108e3810e6794fb68b189d8b <- deobfuscated
	//          <----><----><><-------------------------------------->
	//           /      |    \                   |
	//      varint   varint   varint          script <- P2PKH/P2SH hash160, P2PK public key, or complete script
	//         |        |     nSize
	//         |        |
	//         |     amount (compressesed)
	//         |
	//         |
	//  100000100001010100110
	//  <------------------> \
	//         height         coinbase

	offset := 0

	// First Varint
	// ------------
	// b98276a2ec7700cbc2986ff9aed6825920aece14aa6f5382ca5580
	// <---->
	varint, bytesRead := btcleveldb.Varint128Read(xor, offset) // start reading at 0
	offset += bytesRead
	varintDecoded := btcleveldb.Varint128Decode(varint)

	// Height (first bits)
	output.Height = varintDecoded >> 1 // right-shift to remove last bit

	// Coinbase (last bit)
	output.Coinbase = varintDecoded&1 == 1 // AND to extract right-most bit

	// Second Varint
	// -------------
	// b98276a2ec7700cbc2986ff9aed6825920aece14aa6f5382ca5580
	//       <---->
	varint, bytesRead = btcleveldb.Varint128Read(xor, offset) // start after last varint
	offset += bytesRead
	varintDecoded = btcleveldb.Varint128Decode(varint)

	// Amount
	output.Amount = btcleveldb.DecompressValue(varintDecoded) // int64

	// Third Varint
	// ------------
	// b98276a2ec7700cbc2986ff9aed6825920aece14aa6f5382ca5580
	//             <>
	//
	// nSize - byte to indicate the type or size of script - helps with compression of the script data
	//  - https://github.com/bitcoin/bitcoin/blob/master/src/compressor.cpp

	//  0  = P2PKH <- hash160 public key
	//  1  = P2SH  <- hash160 script
	//  2  = P2PK 02publickey <- nsize makes up part of the public key in the actual script
	//  3  = P2PK 03publickey
	//  4  = P2PK 04publickey (uncompressed - but has been compressed in to leveldb) y=even
	//  5  = P2PK 04publickey (uncompressed - but has been compressed in to leveldb) y=odd
	//  6+ = [size of the upcoming script] (subtract 6 though to get the actual size in bytes, to account for the previous 5 script types already taken)
	varint, bytesRead = btcleveldb.Varint128Read(xor, offset) // start after last varint
	offset += bytesRead
	nsize := btcleveldb.Varint128Decode(varint) //

	// Script (remaining bytes)
	// ------
	// b98276a2ec7700cbc2986ff9aed6825920aece14aa6f5382ca5580
	//               <-------------------------------------->
	if nsize > 1 && nsize < 6 { // either 2, 3, 4, 5
		// move offset back a byte if script type is 2, 3, 4, or 5 (because this forms part of the P2PK public key along with the actual script)
		offset--
	}

	script := xor[offset:]
	output.Script = hex.EncodeToString(script)

	// Addresses - Get address from script (if possible), and set script type (P2PK, P2PKH, P2SH, P2MS, P2WPKH, or P2WSH)
	// ---------

	var address string // initialize address variable

	if len(script) > 0 && script[0] == 0x6a { // OP_RETURN = 0x6a
		// nulldata
		output.Type = NULLDATA
		return output, nil
	}

	if isTrue, _ := txscript.IsMultisigScript(script); isTrue { // P2MS
		output.Type = MULTISIG
		return output, nil
	}
	if nsize < 6 { // legacy txout types
		if nsize == 0 { // P2PKH
			if testnet == true {
				address = keys.Hash160ToAddress(script, []byte{0x6f}) // (m/n)address - testnet addresses have a special prefix
			} else {
				address = keys.Hash160ToAddress(script, []byte{0x00}) // 1address
			}
			output.Type = PUBKEYHASH
			output.Address = address
			return output, nil
		}
		if nsize == 1 { // P2SH
			if testnet == true {
				address = keys.Hash160ToAddress(script, []byte{0xc4}) // 2address - testnet addresses have a special prefix
			} else {
				address = keys.Hash160ToAddress(script, []byte{0x05}) // 3address
			}
			output.Type = SCRIPTHASH
			output.Address = address
			return output, nil
		}

		// P2PK
		// 2, 3, 4, 5
		//  2 = P2PK 02publickey <- nsize makes up part of the public key in the actual script (e.g. 02publickey)
		//  3 = P2PK 03publickey <- y is odd/even (0x02 = even, 0x03 = odd)
		//  4 = P2PK 04publickey (uncompressed)  y = odd  <- actual script uses an uncompressed public key, but it is compressed when stored in this db
		//  5 = P2PK 04publickey (uncompressed) y = even
		// "The uncompressed pubkeys are compressed when they are added to the db. 0x04 and 0x05 are used to indicate that the key is supposed to be uncompressed and those indicate whether the y value is even or odd so that the full uncompressed key can be retrieved."
		//
		// if nsize is 4 or 5, you will need to uncompress the public key to get it's full form
		// if nsize == 4 || nsize == 5 {
		//     // uncompress (4 = y is even, 5 = y is odd)
		//     script = decompress(script)
		// }
		output.Type = PUBKEY
		// the script is also the pubkey
		return output, nil
	}

	if txscript.IsWitnessProgram(script) { // witness program
		version := script[0]
		if version > 0x50 {
			version -= 0x50
		}
		program := script[2:]

		// bech32 function takes an int array and not a byte array, so convert the array to integers
		var programint []int // initialize empty integer array to hold the new one
		for _, v := range program {
			programint = append(programint, int(v)) // cast every value to an int
		}

		if testnet == true {
			address, _ = bech32.SegwitAddrEncode("tb", int(version), programint) // hrp (string), version (int), program ([]int)
		} else {
			address, _ = bech32.SegwitAddrEncode("bc", int(version), programint) // hrp (string), version (int), program ([]int)
		}

		if nsize == 28 && version == 0 && script[1] == 20 { // P2WPKH (script type is 28, which means length of script is 22 bytes)
			output.Type = WITNESS_V0_KEYHASH
			output.Address = address
			return output, nil
		}
		if nsize == 40 && version == 0 && script[1] == 32 { // P2WSH (script type is 40, which means length of script is 34 bytes)
			output.Type = WITNESS_V0_SCRIPTHASH
			output.Address = address
			return output, nil
		}
		if nsize == 40 && version == 1 && script[1] == 32 { // P2TR
			output.Type = WITNESS_V1_TAPROOT
			output.Address = address
			return output, nil
		}
		output.Type = WITNESS_UNKNOWN
		output.Address = address
		return output, nil
	}

	// add address and script type to results map
	output.Type = NONSTANDARD
	return output, nil
}

func insertUTXOToMongo(ctx context.Context, docs []interface{}) error {
	if res := utxoCollection.FindOne(ctx, bson.M{"_id": docs[0].(*UTXO).ID}); res.Err() == nil {
		// if the first records is found then abort this task immediately
		log.Println("[info] already done for batch starting with", docs[0].(*UTXO).ID)
		return nil
	}
	session, err := mongoCli.StartSession()
	if err != nil {
		log.Println("[fatal] failed to start a new session with error: ", err.Error(), " now quitting")
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		return err
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(sessionContext mongo.SessionContext) (interface{}, error) {
		if _, err := utxoCollection.InsertMany(sessionContext, docs); err != nil && !mongo.IsDuplicateKeyError(err) {
			return nil, err
		}
		return nil, nil
	})
	return err
}

func processFailed(ctx context.Context) {
	fileInfos, err := ioutil.ReadDir(pathForFailed)
	if err != nil {
		log.Println("[fatal] failed to read directory", pathForFailed, "with error:", err.Error(), " now quitting")
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		return
	}
	totalFailed := len(fileInfos)
	log.Println("[info] total failed:", totalFailed)
	finished := 0
	var fi fs.FileInfo
	for len(fileInfos) > 0 {
		fi, fileInfos = fileInfos[0], fileInfos[1:]
		filepath := path.Join(pathForFailed, fi.Name())
		file, err := os.Open(filepath)
		if err != nil {
			log.Println("[error] failed to open file", filepath, "with error:", err.Error())
			fileInfos = append(fileInfos, fi)
			continue
		}
		var utxos []*UTXO
		if err = json.NewDecoder(file).Decode(&utxos); err != nil {
			file.Close()
			log.Println("[error] failed to decode", filepath, "with error:", err.Error())
			fileInfos = append(fileInfos, fi)
			continue
		}
		docs := make([]interface{}, len(utxos))
		for index, utxo := range utxos {
			docs[index] = utxo
		}
		if err = insertUTXOToMongo(ctx, docs); err != nil {
			file.Close()
			log.Println("[error] failed to insert again with error:", err.Error())
			fileInfos = append(fileInfos, fi)
			continue
		}
		finished++
		log.Println("[info] successfully finished processing", filepath)
		log.Printf("[info] finished processing %d/%d\n", finished, totalFailed)
		file.Close()
		os.Remove(filepath)
	}
	log.Println("[info] successfully finished processing all failed")
}
