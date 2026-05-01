<template>
  <div class="upload-container">
    <el-upload
      class="upload-demo"
      drag
      action=""
      :auto-upload="false"
      :on-change="handleFileChange"
      :show-file-list="false"
    >
      <el-icon class="el-icon--upload"><UploadFilled /></el-icon>
      <div class="el-upload__text">
        将文件拖到此处，或 <em>点击上传</em>
      </div>
    </el-upload>

    <div v-if="file" class="file-status">
      <p>{{ file.name }} ({{ formatSize(file.size) }})</p>
      <el-progress :percentage="progress" />
      <div class="speed-display" v-if="uploading || paused || uploaded">
        <span>实时速度：{{ currentSpeedMbps }} Mbps</span>
        <span>参考带宽：{{ FIXED_BANDWIDTH_MBPS }} Mbps</span>
        <span>当前利用率：{{ currentBandwidthUtilization }}%</span>
        <span>分片大小：{{ formatSize(UPLOAD_CHUNK_SIZE) }}</span>
      </div>

      <div class="bandwidth-panel" v-if="uploading || paused || uploaded">
        <div class="metric-strip">
          <div class="metric-item">
            <span class="metric-label">平均速度</span>
            <strong>{{ averageConfirmedSpeedMbps }} Mbps</strong>
          </div>
          <div class="metric-item">
            <span class="metric-label">平均利用率</span>
            <strong>{{ averageBandwidthUtilization }}%</strong>
          </div>
        </div>

        <div class="speed-chart" v-if="speedChartPolyline">
          <div class="chart-header">
            <span>实时速度曲线</span>
            <span>上传全程</span>
          </div>
          <svg viewBox="0 0 600 120" role="img" aria-label="上传全程实时速度曲线">
            <line x1="0" y1="119" x2="600" y2="119" class="chart-axis" />
            <polyline :points="speedChartPolyline" class="chart-line" />
          </svg>
        </div>
      </div>

      <div class="actions">
        <el-button type="primary" @click="startUpload" v-if="!uploading && !paused && !uploaded">
          开始上传
        </el-button>
        <el-button type="warning" @click="pauseUpload" v-if="uploading">
          暂停
        </el-button>
        <el-button type="primary" @click="resumeUpload" v-if="paused">
          继续
        </el-button>
        <el-button type="danger" @click="stopUpload" v-if="paused">
          停止上传
        </el-button>
      </div>
    </div>

    <div v-if="uploaded" class="result-box">
      <el-result
        icon="success"
        title="上传成功"
        sub-title="您的文件已经准备好分享"
      >
        <template #extra>
          <div class="code-display">
            <span>取件码</span>
            <h1 class="code">{{ shareCode }}</h1>
          </div>
          <div v-if="uploadStats" class="stats-display">
            <div class="stats-title">上传测试记录</div>
            <p>开始时间：{{ formatDateTime(uploadStats.startedAt) }}</p>
            <p>结束时间：{{ formatDateTime(uploadStats.finishedAt) }}</p>
            <p>上传耗时：{{ uploadStats.durationText }}</p>
            <p>确认传输量：{{ formatSize(uploadStats.confirmedBytes) }}</p>
            <p>平均确认速度：{{ uploadStats.averageSpeedMbps }} Mbps</p>
            <p>有效带宽利用率：{{ uploadStats.averageBandwidthUtilization }}%</p>
          </div>
        </template>
      </el-result>
    </div>
  </div>
</template>

<script setup>
import { computed, ref } from 'vue'
import * as tus from 'tus-js-client'
import axios from 'axios'
import { UploadFilled } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { getApiKey, getAdminKey } from '../config.js'

const file = ref(null)
const progress = ref(0)
const uploading = ref(false)
const paused = ref(false)
const uploaded = ref(false)
const shareCode = ref('')
const uploadStats = ref(null)
const uploadStartedAt = ref(null)
const currentSpeedMbps = ref('0.00')
const currentBandwidthUtilization = ref('0.0')
const averageConfirmedSpeedMbps = ref('0.00')
const averageBandwidthUtilization = ref('0.0')
const speedHistory = ref([])

const UPLOAD_CHUNK_SIZE = 64 * 1024 * 1024
const SPEED_WINDOW_MS = 10 * 1000
const UI_PROGRESS_UPDATE_MS = 500
const SPEED_HISTORY_FINE_SAMPLE_MS = 30 * 1000
const SPEED_HISTORY_FINE_WINDOW_MS = 60 * 60 * 1000
const SPEED_HISTORY_COARSE_SAMPLE_MS = 5 * 60 * 1000
const SPEED_HISTORY_MAX_POINTS = 500
const SPEED_CHART_WIDTH = 600
const SPEED_CHART_HEIGHT = 120
const FIXED_BANDWIDTH_MBPS = 100
const PARALLEL_UPLOADS = 4

let upload = null
let speedSamples = []
let realtimeBytesUploaded = 0
let confirmedBytesUploaded = 0
let activeUploadStartedAtMs = null
let activeUploadDurationMs = 0
let lastSpeedHistorySampleAt = 0
let lastProgressUiUpdateAt = 0

const speedChartPolyline = computed(() => {
  if (speedHistory.value.length < 2) return ''

  const latest = speedHistory.value[speedHistory.value.length - 1].time
  const started = speedHistory.value[0].time
  const duration = Math.max(1, latest - started)
  const maxSpeed = Math.max(
    FIXED_BANDWIDTH_MBPS,
    ...speedHistory.value.map(sample => sample.mbps)
  )

  return speedHistory.value
    .map((sample) => {
      const x = ((sample.time - started) / duration) * SPEED_CHART_WIDTH
      const y = SPEED_CHART_HEIGHT - Math.min(sample.mbps / maxSpeed, 1) * (SPEED_CHART_HEIGHT - 2) - 1
      return `${Math.max(0, Math.min(SPEED_CHART_WIDTH, x)).toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
})

const resetUploadState = () => {
  progress.value = 0
  uploading.value = false
  paused.value = false
  uploaded.value = false
  shareCode.value = ''
  uploadStats.value = null
  uploadStartedAt.value = null
  resetTransferMetrics()
}

const handleFileChange = (uploadFile) => {
  file.value = uploadFile.raw
  upload = null
  resetUploadState()
}

const formatSize = (bytes) => {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i]
}

const formatDateTime = (date) => {
  if (!date) return '-'
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  }).format(date)
}

const formatDuration = (durationMs) => {
  const totalSeconds = Math.max(0, Math.round(durationMs / 1000))
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60
  const parts = []

  if (hours > 0) parts.push(`${hours}小时`)
  if (minutes > 0 || hours > 0) parts.push(`${minutes}分`)
  parts.push(`${seconds}秒`)

  return parts.join(' ')
}

const formatSpeedMbps = (value) => {
  return Number.isFinite(value) && value > 0 ? value.toFixed(2) : '0.00'
}

const formatUtilization = (speedMbps) => {
  const utilization = (speedMbps / FIXED_BANDWIDTH_MBPS) * 100
  return Number.isFinite(utilization) && utilization > 0 ? Math.min(100, utilization).toFixed(1) : '0.0'
}

const buildHeaders = () => {
  const apiKey = getApiKey()
  const adminKey = getAdminKey()
  const headers = {}

  if (apiKey) {
    headers['X-API-Key'] = apiKey
  } else if (adminKey) {
    headers['X-Admin-Key'] = adminKey
  }

  return headers
}

const resetTransferMetrics = () => {
  currentSpeedMbps.value = '0.00'
  currentBandwidthUtilization.value = '0.0'
  averageConfirmedSpeedMbps.value = '0.00'
  averageBandwidthUtilization.value = '0.0'
  speedHistory.value = []
  speedSamples = []
  realtimeBytesUploaded = 0
  confirmedBytesUploaded = 0
  activeUploadStartedAtMs = null
  activeUploadDurationMs = 0
  lastSpeedHistorySampleAt = 0
  lastProgressUiUpdateAt = 0
}

const compactSpeedHistory = (history, now) => {
  const fineCutoff = now - SPEED_HISTORY_FINE_WINDOW_MS
  const coarseBuckets = new Map()
  const recentSamples = []

  for (const sample of history) {
    if (!Number.isFinite(sample.time) || !Number.isFinite(sample.mbps)) {
      continue
    }

    if (sample.time >= fineCutoff) {
      recentSamples.push(sample)
      continue
    }

    const bucket = Math.floor(sample.time / SPEED_HISTORY_COARSE_SAMPLE_MS)
    const existing = coarseBuckets.get(bucket)
    if (!existing || sample.time > existing.time) {
      coarseBuckets.set(bucket, sample)
    }
  }

  const compacted = [
    ...Array.from(coarseBuckets.values()).sort((a, b) => a.time - b.time),
    ...recentSamples
  ]

  return compacted.length > SPEED_HISTORY_MAX_POINTS
    ? compacted.slice(compacted.length - SPEED_HISTORY_MAX_POINTS)
    : compacted
}

const appendSpeedHistory = (sample, force = false) => {
  if (!force && lastSpeedHistorySampleAt && sample.time - lastSpeedHistorySampleAt < SPEED_HISTORY_FINE_SAMPLE_MS) {
    return
  }

  lastSpeedHistorySampleAt = sample.time
  speedHistory.value = compactSpeedHistory([
    ...speedHistory.value,
    sample
  ], sample.time)
}

const resetSpeedWindow = (now = performance.now()) => {
  currentSpeedMbps.value = '0.00'
  currentBandwidthUtilization.value = '0.0'
  speedSamples = [{ time: now, bytes: realtimeBytesUploaded }]
  appendSpeedHistory({ time: now, mbps: 0 }, true)
}

const beginActiveUploadTiming = () => {
  const now = performance.now()

  if (!uploadStartedAt.value) {
    uploadStartedAt.value = new Date()
  }

  if (activeUploadStartedAtMs === null) {
    activeUploadStartedAtMs = now
  }

  resetSpeedWindow(now)
}

const stopActiveUploadTiming = () => {
  if (activeUploadStartedAtMs === null) return

  activeUploadDurationMs += performance.now() - activeUploadStartedAtMs
  activeUploadStartedAtMs = null
}

const getActiveUploadDurationMs = () => {
  if (activeUploadStartedAtMs === null) return activeUploadDurationMs

  return activeUploadDurationMs + performance.now() - activeUploadStartedAtMs
}

const updateAverageMetrics = () => {
  const durationMs = getActiveUploadDurationMs()
  const averageSpeed = durationMs > 0 && realtimeBytesUploaded > 0
    ? (realtimeBytesUploaded * 8) / (durationMs / 1000) / 1000 / 1000
    : 0

  averageConfirmedSpeedMbps.value = formatSpeedMbps(averageSpeed)
  averageBandwidthUtilization.value = formatUtilization(averageSpeed)
}

const updateRealtimeSpeed = (bytesUploaded, now = performance.now()) => {
  if (!Number.isFinite(bytesUploaded) || bytesUploaded < 0) return

  realtimeBytesUploaded = bytesUploaded
  speedSamples.push({ time: now, bytes: realtimeBytesUploaded })
  speedSamples = speedSamples.filter(sample => now - sample.time <= SPEED_WINDOW_MS)

  if (speedSamples.length < 2) {
    currentSpeedMbps.value = '0.00'
    return
  }

  const first = speedSamples[0]
  const last = speedSamples[speedSamples.length - 1]
  const elapsedSeconds = (last.time - first.time) / 1000
  const realtimeBytes = last.bytes - first.bytes

  const speedMbps = elapsedSeconds > 0 && realtimeBytes > 0
    ? (realtimeBytes * 8) / elapsedSeconds / 1000 / 1000
    : 0

  currentSpeedMbps.value = formatSpeedMbps(speedMbps)
  currentBandwidthUtilization.value = formatUtilization(speedMbps)

  appendSpeedHistory({ time: now, mbps: speedMbps })
  updateAverageMetrics()
}

const updateUploadProgress = (bytesUploaded, bytesTotal, force = false) => {
  if (!Number.isFinite(bytesUploaded) || !Number.isFinite(bytesTotal) || bytesTotal <= 0) return

  const now = performance.now()
  if (!force && lastProgressUiUpdateAt && now - lastProgressUiUpdateAt < UI_PROGRESS_UPDATE_MS) {
    return
  }

  lastProgressUiUpdateAt = now
  const percentage = (bytesUploaded / bytesTotal * 100).toFixed(2)
  progress.value = Math.min(100, Number(percentage))
  updateRealtimeSpeed(bytesUploaded, now)
}

const updateConfirmedSpeed = (chunkSize) => {
  if (!chunkSize || chunkSize <= 0) return

  confirmedBytesUploaded += chunkSize
}

const finishStats = () => {
  stopActiveUploadTiming()

  const finishedAt = new Date()
  const startedAt = uploadStartedAt.value || finishedAt
  const durationMs = Math.max(0, getActiveUploadDurationMs())

  const statsBytes = confirmedBytesUploaded > 0 ? confirmedBytesUploaded : realtimeBytesUploaded
  const averageSpeedMbps = durationMs > 0 && statsBytes > 0
    ? (statsBytes * 8) / (durationMs / 1000) / 1000 / 1000
    : 0

  averageConfirmedSpeedMbps.value = formatSpeedMbps(averageSpeedMbps)
  averageBandwidthUtilization.value = formatUtilization(averageSpeedMbps)

  uploadStats.value = {
    startedAt,
    finishedAt,
    durationText: formatDuration(durationMs),
    confirmedBytes: statsBytes,
    averageSpeedMbps: formatSpeedMbps(averageSpeedMbps),
    averageBandwidthUtilization: formatUtilization(averageSpeedMbps)
  }
}

const applyServerUploadMetric = (metric) => {
  if (!metric || !uploadStats.value) return

  const confirmedBytes = Number(metric.bytes)
  const durationMs = Number(metric.duration_ms)
  if (!Number.isFinite(confirmedBytes) || !Number.isFinite(durationMs) || confirmedBytes <= 0 || durationMs <= 0) {
    return
  }

  const serverStartedAt = metric.started_at ? new Date(metric.started_at) : null
  const serverFinishedAt = metric.finished_at ? new Date(metric.finished_at) : null
  const averageSpeed = Number.isFinite(Number(metric.average_mbps)) && Number(metric.average_mbps) > 0
    ? Number(metric.average_mbps)
    : (confirmedBytes * 8) / (durationMs / 1000) / 1000 / 1000
  const utilization = Number.isFinite(Number(metric.bandwidth_utilization)) && Number(metric.bandwidth_utilization) > 0
    ? Math.min(100, Number(metric.bandwidth_utilization))
    : Number(formatUtilization(averageSpeed))
  averageConfirmedSpeedMbps.value = formatSpeedMbps(averageSpeed)
  averageBandwidthUtilization.value = Number.isFinite(utilization) ? utilization.toFixed(1) : formatUtilization(averageSpeed)

  uploadStats.value = {
    ...uploadStats.value,
    startedAt: serverStartedAt && !Number.isNaN(serverStartedAt.getTime()) ? serverStartedAt : uploadStats.value.startedAt,
    finishedAt: serverFinishedAt && !Number.isNaN(serverFinishedAt.getTime()) ? serverFinishedAt : uploadStats.value.finishedAt,
    durationText: formatDuration(durationMs),
    confirmedBytes,
    averageSpeedMbps: formatSpeedMbps(averageSpeed),
    averageBandwidthUtilization: averageBandwidthUtilization.value
  }
}

const uploadIdFromUrl = (url) => {
  if (!url) return ''

  try {
    const parsed = new URL(url, window.location.origin)
    const parts = parsed.pathname.split('/').filter(Boolean)
    return parts[parts.length - 1] || ''
  } catch {
    const cleanUrl = url.split('?')[0]
    return cleanUrl.substring(cleanUrl.lastIndexOf('/') + 1)
  }
}

const terminateUploadOnServer = async (uploadId) => {
  if (!uploadId) return

  await axios.delete(`/files/${encodeURIComponent(uploadId)}`, {
    headers: buildHeaders()
  })
}

const startUpload = () => {
  if (!file.value) return

  uploading.value = true
  paused.value = false
  uploaded.value = false
  uploadStats.value = null
  uploadStartedAt.value = null
  resetTransferMetrics()

  const headers = buildHeaders()

  upload = new tus.Upload(file.value, {
    endpoint: '/files/',
    chunkSize: UPLOAD_CHUNK_SIZE,
    parallelUploads: PARALLEL_UPLOADS,
    retryDelays: [0, 3000, 5000, 10000, 20000],
    headers,
    metadata: {
      filename: file.value.name,
      filetype: file.value.type
    },
    onError: (error) => {
      console.error('Failed because:', error)
      ElMessage.error('上传失败: ' + error.message)
      stopActiveUploadTiming()
      uploading.value = false
    },
    onProgress: (bytesUploaded, bytesTotal) => {
      updateUploadProgress(bytesUploaded, bytesTotal)
    },
    onChunkComplete: (chunkSize) => {
      updateConfirmedSpeed(chunkSize)
    },
    onSuccess: async () => {
      const totalBytes = file.value?.size || realtimeBytesUploaded
      updateUploadProgress(totalBytes, totalBytes, true)
      uploading.value = false
      uploaded.value = true
      finishStats()

      const uploadUrl = upload.url
      const uploadId = uploadUrl.substring(uploadUrl.lastIndexOf('/') + 1)

      try {
        const res = await axios.post('/api/get-code', { upload_id: uploadId })
        shareCode.value = res.data.code
        applyServerUploadMetric(res.data.upload_metric)
      } catch (err) {
        console.error('Generate share code error:', err)
        ElMessage.error('生成取件码失败')
      }
    }
  })

  upload.findPreviousUploads().then((previousUploads) => {
    if (previousUploads.length) {
      upload.resumeFromPreviousUpload(previousUploads[0])
    }
    beginActiveUploadTiming()
    upload.start()
  })
}

const pauseUpload = () => {
  if (upload) {
    upload.abort()
    stopActiveUploadTiming()
    uploading.value = false
    paused.value = true
    resetSpeedWindow()
  }
}

const resumeUpload = () => {
  if (upload) {
    beginActiveUploadTiming()
    upload.start()
    uploading.value = true
    paused.value = false
  }
}

const stopUpload = async () => {
  if (!upload) {
    resetUploadState()
    return
  }

  const uploadId = uploadIdFromUrl(upload.url)
  stopActiveUploadTiming()

  try {
    try {
      await upload.abort(true)
    } catch (err) {
      if (uploadId) {
        await terminateUploadOnServer(uploadId)
      } else {
        throw err
      }
    }

    upload = null
    resetUploadState()
    ElMessage.success('已停止上传')
  } catch (err) {
    uploading.value = false
    paused.value = true
    ElMessage.error('停止上传失败: ' + (err.response?.data?.error || err.message))
  }
}
</script>

<style scoped>
.upload-container {
  text-align: center;
  padding: 20px;
}

.file-status {
  margin-top: 20px;
}

.actions {
  margin-top: 15px;
}

.speed-display {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: 18px;
  margin-top: 10px;
  color: #606266;
  font-size: 13px;
}

.bandwidth-panel {
  margin: 14px auto 0;
  max-width: 760px;
}

.metric-strip {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  text-align: left;
  border-top: 1px solid #ebeef5;
  border-bottom: 1px solid #ebeef5;
}

.metric-item {
  min-width: 0;
  padding: 10px 12px;
  border-right: 1px solid #ebeef5;
}

.metric-item:last-child {
  border-right: 0;
}

.metric-label {
  display: block;
  margin-bottom: 4px;
  color: #909399;
  font-size: 12px;
}

.metric-item strong {
  display: block;
  color: #303133;
  font-size: 16px;
  font-weight: 600;
  word-break: break-word;
}

.speed-chart {
  margin-top: 12px;
  padding: 10px 12px 12px;
  border: 1px solid #dcdfe6;
  border-radius: 6px;
  background: #fff;
}

.chart-header {
  display: flex;
  justify-content: space-between;
  margin-bottom: 8px;
  color: #606266;
  font-size: 12px;
}

.speed-chart svg {
  display: block;
  width: 100%;
  height: 120px;
}

.chart-axis {
  stroke: #e4e7ed;
  stroke-width: 1;
}

.chart-line {
  fill: none;
  stroke: #409eff;
  stroke-width: 3;
  stroke-linecap: round;
  stroke-linejoin: round;
}

@media (max-width: 640px) {
  .metric-strip {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .metric-item:nth-child(2n) {
    border-right: 0;
  }
}

.code-display {
  margin-top: 10px;
  background: #f0f9eb;
  padding: 15px;
  border-radius: 8px;
  border: 1px solid #67c23a;
}

.code {
  color: #67c23a;
  font-size: 18px;
  margin: 10px 0;
  word-break: break-all;
  font-family: monospace;
}

.stats-display {
  margin-top: 16px;
  text-align: left;
  background: #f4f4f5;
  padding: 15px;
  border-radius: 8px;
  border: 1px solid #dcdfe6;
}

.stats-display p {
  margin: 8px 0;
}

.stats-title {
  font-weight: 600;
  margin-bottom: 10px;
}
</style>
