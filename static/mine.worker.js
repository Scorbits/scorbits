var hashCount = 0;
var miningStartTime = 0;
var wasmReady = false;
var miningActive = false;
var pendingJob = null;

// État du batch courant
var batchNonce = 0;
var batchJob = null;

self.importScripts('/static/wasm_exec.js');

async function loadWasm() {
    const go = new Go();
    try {
        const result = await WebAssembly.instantiateStreaming(
            fetch('/static/miner.wasm?v='+Date.now()),
            go.importObject
        );
        go.run(result.instance);

        var attempts = 0;
        var checkReady = setInterval(function() {
            attempts++;
            if (typeof self.goMineBlockBatch === 'function') {
                clearInterval(checkReady);
                wasmReady = true;
                self.postMessage({ type: 'ready' });
                if (pendingJob) {
                    startMining(pendingJob);
                    pendingJob = null;
                }
            } else if (attempts >= 100) {
                clearInterval(checkReady);
                self.postMessage({ type: 'error', message: 'goMineBlockBatch non disponible' });
            }
        }, 100);

    } catch (err) {
        self.postMessage({ type: 'error', message: 'Erreur WASM: ' + err.message });
    }
}

self.onmessage = function(e) {
    switch (e.data.type) {
        case 'init':
            loadWasm();
            break;
        case 'start':
            hashCount = 0;
            miningStartTime = Date.now();
            miningActive = true;
            if (wasmReady) {
                startMining(e.data.data);
            } else {
                pendingJob = e.data.data;
            }
            break;
        case 'newjob':
            // Nouveau job sans reset des compteurs — hashrate continu entre les blocs
            miningActive = true;
            var data = e.data.data;
            var job = data.job;
            batchJob = {
                index: job.last_index + 1,
                transactions: job.transactions || [],
                previousHash: job.last_hash,
                minerAddress: data.minerAddress,
                difficulty: job.difficulty
            };
            batchNonce = 0;
            setTimeout(runBatch, 0);
            break;
        case 'stop':
            miningActive = false;
            self.postMessage({ type: 'stopped' });
            break;
    }
};

function startMining(data) {
    var job = data.job;
    var minerAddress = data.minerAddress;

    batchJob = {
        index: job.last_index + 1,
        transactions: job.transactions || [],
        previousHash: job.last_hash,
        minerAddress: minerAddress,
        difficulty: job.difficulty
    };
    batchNonce = 0;

    setTimeout(runBatch, 0);
}

function runBatch() {
    if (!miningActive) return;

    var j = batchJob;
    var currentTimestamp = Math.floor(Date.now() / 1000);
    var batchStart = Date.now();
    var result = self.goMineBlockBatch(
        j.index,
        j.transactions,
        j.previousHash,
        j.minerAddress,
        j.difficulty,
        batchNonce,
        currentTimestamp
    );

    var batchMs = Date.now() - batchStart;
    if (batchMs > 0) {
        var instantHashrate = (result.hashes || 10000) / (batchMs / 1000);
        self.postMessage({ type: 'hashrate', hashrate: instantHashrate });
    }

    if (result.found) {
        miningActive = false;
        self.postMessage({
            type: 'found',
            hash: result.hash,
            nonce: result.nonce,
            timestamp: result.timestamp
        });
    } else {
        batchNonce = result.nextNonce;
        setTimeout(runBatch, 0);
    }
}
