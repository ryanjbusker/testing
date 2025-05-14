class AudioProcessor extends AudioWorkletProcessor {
    constructor() {
        super();
        this.sampleRate = 48000;
        this.targetSampleRate = 16000;
        this.buffer = [];
        this.chunkSize = this.sampleRate / 10; // 100ms at 48kHz
        this.port.postMessage({ 
            type: 'init', 
            sampleRate: this.sampleRate, 
            targetSampleRate: this.targetSampleRate,
            chunkSize: this.chunkSize
        });
    }

    process(inputs, outputs) {
        const input = inputs[0];
        if (!input || input.length === 0) return true;

        // Log input format
        this.port.postMessage({ 
            type: 'input', 
            channels: input.length,
            samples: input[0].length,
            sampleRate: this.sampleRate,
            bufferSize: this.buffer.length
        });

        // Convert stereo to mono and collect samples
        for (let i = 0; i < input[0].length; i++) {
            // Average left and right channels
            const mono = (input[0][i] + input[1][i]) / 2;
            this.buffer.push(mono);
        }

        // Process when we have enough samples
        while (this.buffer.length >= this.chunkSize) {
            // Log before processing
            this.port.postMessage({ 
                type: 'processing', 
                bufferSize: this.buffer.length,
                expectedOutputSamples: Math.round(this.chunkSize * this.targetSampleRate / this.sampleRate)
            });

            // Take exactly chunkSize samples
            const chunk = this.buffer.slice(0, this.chunkSize);
            this.buffer = this.buffer.slice(this.chunkSize);

            // Resample from 48kHz to 16kHz
            const resampled = this.resample(chunk, this.sampleRate, this.targetSampleRate);
            
            // Convert to Int16
            const int16Array = new Int16Array(resampled.length);
            for (let i = 0; i < resampled.length; i++) {
                int16Array[i] = Math.max(-1, Math.min(1, resampled[i])) * 0x7FFF;
            }

            // Log output format
            this.port.postMessage({ 
                type: 'output', 
                samples: int16Array.length,
                sampleRate: this.targetSampleRate,
                bytes: int16Array.byteLength,
                duration: (int16Array.length / this.targetSampleRate * 1000).toFixed(1) + 'ms'
            });

            // Send the processed audio
            this.port.postMessage(int16Array.buffer, [int16Array.buffer]);
        }

        return true;
    }

    resample(input, inputSampleRate, outputSampleRate) {
        const ratio = inputSampleRate / outputSampleRate;
        const outputLength = Math.round(input.length / ratio);
        const output = new Float32Array(outputLength);

        for (let i = 0; i < outputLength; i++) {
            const inputIndex = i * ratio;
            const inputIndexFloor = Math.floor(inputIndex);
            const inputIndexCeil = Math.min(input.length - 1, Math.ceil(inputIndex));
            const fraction = inputIndex - inputIndexFloor;

            output[i] = input[inputIndexFloor] * (1 - fraction) + input[inputIndexCeil] * fraction;
        }

        return output;
    }
}

registerProcessor('audio-processor', AudioProcessor); 