package eth

import (
    "math/big"

    "golang.org/x/crypto/sha3"
)


// Conversion factor from wei to eth
const WEI_PER_ETH float64 = 1000000000000000000


// Convert wei to eth
func WeiToEth(wei *big.Int) float64 {
    var weiFloat big.Float
    var eth big.Float
    weiFloat.SetInt(wei)
    eth.Quo(&weiFloat, big.NewFloat(WEI_PER_ETH))
    eth64, _ := eth.Float64()
    return eth64
}


// Convert eth to wei
func EthToWei(eth float64) *big.Int {
    var weiFloat big.Float
    var wei big.Int
    weiFloat.Mul(big.NewFloat(eth), big.NewFloat(WEI_PER_ETH))
    weiFloat.Int(&wei)
    return &wei
}


// Make a keccak256 hash of a source byte slice and return as a 32-byte array
func KeccakBytes(src []byte) [32]byte {

    // Hash source data
    hash := sha3.NewLegacyKeccak256()
    hash.Write(src[:])

    // Copy hashed data to byte array
    var bytes [32]byte
    copy(bytes[:], hash.Sum(nil))

    // Return
    return bytes

}


// Make a keccak256 hash of a source string and return as a 32-byte array
func KeccakStr(src string) [32]byte {
    return KeccakBytes([]byte(src))
}

