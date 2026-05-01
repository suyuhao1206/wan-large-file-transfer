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
      <p class="file-summary">{{ file.name }} ({{ formatSize(file.size) }})</p>
      <el-progress :percentage="progress" />

      <div class="bandwidth-panel" v-if="uploading || paused || uploaded">
        <div class="metric-strip">
          <div class="metric-item metric-primary">
            <span class="metric-label">上传速率</span>
            <strong><span v-if="currentSpeedPrefix">{{ currentSpeedPrefix }}</span>{{ currentSpeedText }}<em v-if="showCurrentSpeedUnit">Mbps</em></strong>
          </div>
          <div class="metric-item metric-usage">
            <span class="metric-label">带宽占用</span>
            <strong>{{ averageBandwidthUtilization }}</strong>
            <div class="usage-meter" aria-hidden="true">
              <span :style="{ width: bandwidthUsageWidth }"></span>
            </div>
          </div>
          <div class="metric-item">
            <span class="metric-label">参考带宽</span>
            <strong>{{ FIXED_BANDWIDTH_MBPS }}<em>Mbps</em></strong>
          </div>
          <div class="metric-item">
            <span class="metric-label">预计剩余</span>
            <strong>{{ estimatedRemainingText }}</strong>
          </div>
        </div>

        <div class="speed-chart" v-if="speedChartBars.length">
          <div class="chart-header">
            <span>实时速度</span>
            <span>最近2分钟</span>
          </div>
          <div class="speed-bars" role="img" aria-label="最近2分钟上传速度柱状图">
            <span
              v-for="bar in speedChartBars"
              :key="bar.key"
              :style="{ height: bar.height }"
            ></span>
          </div>
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
      <div class="result-header">
        <div class="result-status">上传成功！</div>
        <div class="result-subtitle">文件已经准备好分享</div>
      </div>

      <div class="code-display">
        <span>取件码</span>
        <strong class="code">{{ shareCode || '生成中' }}</strong>
        <el-button
          class="copy-code-button"
          size="small"
          :disabled="!shareCode"
          @click="copyShareCode"
        >
          <el-icon><CopyDocument /></el-icon>
          <span>复制</span>
        </el-button>
      </div>

      <div v-if="uploadStats" class="stats-display">
        <div class="stats-title">上传记录</div>
        <div class="stats-grid">
          <div class="stats-item">
            <span>开始时间</span>
            <strong>{{ formatDateTime(uploadStats.startedAt) }}</strong>
          </div>
          <div class="stats-item">
            <span>结束时间</span>
            <strong>{{ formatDateTime(uploadStats.finishedAt) }}</strong>
          </div>
          <div class="stats-item">
            <span>上传耗时</span>
            <strong>{{ uploadStats.durationText }}</strong>
          </div>
          <div class="stats-item">
            <span>确认传输量</span>
            <strong>{{ formatSize(uploadStats.confirmedBytes) }}</strong>
          </div>
          <div class="stats-item">
            <span>平均确认速度</span>
            <strong>{{ uploadStats.averageSpeedMbps }} Mbps</strong>
          </div>
          <div class="stats-item">
            <span>有效带宽利用率</span>
            <strong>{{ uploadStats.averageBandwidthUtilization }}%</strong>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, ref } from 'vue'
import * as tus from 'tus-js-client'
import axios from 'axios'
import { CopyDocument, UploadFilled } from '@element-plus/icons-vue'
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
const currentSpeedText = ref('0.00')
const currentSpeedPrefix = ref('约')
const showCurrentSpeedUnit = ref(true)
const averageBandwidthUtilization = ref('0.0%')
const estimatedRemainingText = ref('-')
const speedHistory = ref([])

const UPLOAD_CHUNK_SIZE = 64 * 1024 * 1024
const SPEED_WINDOW_MS = 30 * 1000
const UI_PROGRESS_UPDATE_MS = 500
const SPEED_HISTORY_SAMPLE_MS = 1000
const SPEED_HISTORY_MAX_POINTS = 120
const FIXED_BANDWIDTH_MBPS = 100
const SPEED_DISPLAY_MAX_MBPS = FIXED_BANDWIDTH_MBPS
const FULL_SPEED_DISPLAY_THRESHOLD_MBPS = 98
const SATURATED_UTILIZATION_THRESHOLD = 95
const PARALLEL_UPLOADS = 4

let upload = null
let speedSamples = []
let realtimeBytesUploaded = 0
let displayBytesUploaded = 0
let confirmedBytesUploaded = 0
let activeUploadStartedAtMs = null
let activeUploadDurationMs = 0
let lastSpeedHistorySampleAt = 0
let lastProgressUiUpdateAt = 0

const bandwidthUsageWidth = computed(() => {
  const utilization = uploadDisplayUtilization()
  const safeUtilization = Number.isFinite(utilization) ? utilization : 0
  return `${safeUtilization}%`
})

const speedChartBars = computed(() => {
  if (!speedHistory.value.length) return []

  const maxSpeed = Math.max(
    FIXED_BANDWIDTH_MBPS,
    ...speedHistory.value.map(sample => sample.mbps)
  )

  return speedHistory.value
    .map((sample, index) => {
      const ratio = maxSpeed > 0 ? Math.min(sample.mbps / maxSpeed, 1) : 0
      return {
        key: `${Math.round(sample.time)}-${index}`,
        height: `${Math.max(4, ratio * 100).toFixed(1)}%`
      }
    })
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
  const parts = new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false
  }).formatToParts(date).reduce((result, part) => {
    result[part.type] = part.value
    return result
  }, {})

  return `${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}`
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

const clampDisplaySpeedMbps = (value) => {
  return Number.isFinite(value) && value > 0 ? Math.min(SPEED_DISPLAY_MAX_MBPS, value) : 0
}

const formatDisplaySpeedMbps = (value) => {
  return formatSpeedMbps(clampDisplaySpeedMbps(value))
}

const formatUtilization = (speedMbps) => {
  const utilization = (speedMbps / FIXED_BANDWIDTH_MBPS) * 100
  return Number.isFinite(utilization) && utilization > 0 ? Math.min(100, utilization).toFixed(1) : '0.0'
}

const setDisplaySpeed = (speedMbps) => {
  if (Number.isFinite(speedMbps) && speedMbps >= FULL_SPEED_DISPLAY_THRESHOLD_MBPS) {
    currentSpeedPrefix.value = ''
    currentSpeedText.value = '接近满速'
    showCurrentSpeedUnit.value = false
    return
  }

  currentSpeedPrefix.value = '约'
  currentSpeedText.value = formatSpeedMbps(speedMbps)
  showCurrentSpeedUnit.value = true
}

const formatDisplayUtilization = (speedMbps) => {
  const utilization = (speedMbps / FIXED_BANDWIDTH_MBPS) * 100
  if (!Number.isFinite(utilization) || utilization <= 0) return '0.0%'
  if (utilization >= SATURATED_UTILIZATION_THRESHOLD) return '接近满载'
  return `${utilization.toFixed(1)}%`
}

const uploadDisplayUtilization = () => {
  const durationMs = getActiveUploadDurationMs()
  const averageSpeed = durationMs > 0 && realtimeBytesUploaded > 0
    ? (realtimeBytesUploaded * 8) / (durationMs / 1000) / 1000 / 1000
    : 0
  const utilization = (averageSpeed / FIXED_BANDWIDTH_MBPS) * 100
  return Number.isFinite(utilization) && utilization > 0 ? Math.min(100, utilization) : 0
}

const formatRemainingTime = (durationMs) => {
  if (!Number.isFinite(durationMs) || durationMs <= 0) return '计算中'

  const totalSeconds = Math.ceil(durationMs / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60

  if (hours > 0) return `${hours}小时${minutes}分`
  if (minutes > 0) return `${minutes}分${seconds}秒`
  return `${seconds}秒`
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

const copyShareCode = async () => {
  if (!shareCode.value) return

  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(shareCode.value)
    } else {
      const textarea = document.createElement('textarea')
      textarea.value = shareCode.value
      textarea.setAttribute('readonly', '')
      textarea.style.position = 'fixed'
      textarea.style.opacity = '0'
      document.body.appendChild(textarea)
      textarea.select()
      document.execCommand('copy')
      document.body.removeChild(textarea)
    }

    ElMessage.success('取件码已复制')
  } catch (err) {
    console.error('Copy share code error:', err)
    ElMessage.error('复制失败，请手动复制')
  }
}

const resetTransferMetrics = () => {
  currentSpeedText.value = '0.00'
  currentSpeedPrefix.value = '约'
  showCurrentSpeedUnit.value = true
  averageBandwidthUtilization.value = '0.0%'
  estimatedRemainingText.value = '-'
  speedHistory.value = []
  speedSamples = []
  realtimeBytesUploaded = 0
  displayBytesUploaded = 0
  confirmedBytesUploaded = 0
  activeUploadStartedAtMs = null
  activeUploadDurationMs = 0
  lastSpeedHistorySampleAt = 0
  lastProgressUiUpdateAt = 0
}

const appendSpeedHistory = (sample, force = false) => {
  if (!Number.isFinite(sample.time) || !Number.isFinite(sample.mbps)) {
    return
  }

  if (!force && lastSpeedHistorySampleAt && sample.time - lastSpeedHistorySampleAt < SPEED_HISTORY_SAMPLE_MS) {
    return
  }

  lastSpeedHistorySampleAt = sample.time
  const nextHistory = [
    ...speedHistory.value,
    sample
  ]

  speedHistory.value = nextHistory.length > SPEED_HISTORY_MAX_POINTS
    ? nextHistory.slice(nextHistory.length - SPEED_HISTORY_MAX_POINTS)
    : nextHistory
}

const resetSpeedWindow = (now = performance.now()) => {
  currentSpeedText.value = '0.00'
  currentSpeedPrefix.value = '约'
  showCurrentSpeedUnit.value = true
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

  averageBandwidthUtilization.value = formatDisplayUtilization(averageSpeed)

  if (uploaded.value || progress.value >= 100) {
    estimatedRemainingText.value = '已完成'
    return
  }

  const totalBytes = file.value?.size || 0
  const remainingBytes = Math.max(0, totalBytes - realtimeBytesUploaded)
  if (remainingBytes <= 0) {
    estimatedRemainingText.value = '已完成'
  } else if (averageSpeed > 0) {
    const remainingMs = (remainingBytes * 8) / (averageSpeed * 1000 * 1000) * 1000
    estimatedRemainingText.value = formatRemainingTime(remainingMs)
  } else {
    estimatedRemainingText.value = '计算中'
  }
}

const updateRealtimeSpeed = (bytesUploaded, now = performance.now(), forceHistory = false) => {
  if (!Number.isFinite(bytesUploaded) || bytesUploaded < 0) return

  realtimeBytesUploaded = Math.max(realtimeBytesUploaded, bytesUploaded)
  speedSamples.push({ time: now, bytes: realtimeBytesUploaded })
  speedSamples = speedSamples.filter(sample => now - sample.time <= SPEED_WINDOW_MS)

  if (speedSamples.length < 2) {
    setDisplaySpeed(0)
    return
  }

  const first = speedSamples[0]
  const last = speedSamples[speedSamples.length - 1]
  const elapsedSeconds = (last.time - first.time) / 1000
  const realtimeBytes = last.bytes - first.bytes

  const speedMbps = elapsedSeconds > 0 && realtimeBytes > 0
    ? (realtimeBytes * 8) / elapsedSeconds / 1000 / 1000
    : 0

  const displaySpeedMbps = clampDisplaySpeedMbps(speedMbps)
  setDisplaySpeed(displaySpeedMbps)

  appendSpeedHistory({ time: now, mbps: displaySpeedMbps }, forceHistory || speedHistory.value.length <= 1)
  updateAverageMetrics()
}

const updateUploadProgress = (bytesUploaded, bytesTotal, force = false) => {
  if (!Number.isFinite(bytesUploaded) || !Number.isFinite(bytesTotal) || bytesTotal <= 0) return

  const now = performance.now()
  if (!force && lastProgressUiUpdateAt && now - lastProgressUiUpdateAt < UI_PROGRESS_UPDATE_MS) {
    return
  }

  lastProgressUiUpdateAt = now
  displayBytesUploaded = Math.max(displayBytesUploaded, bytesUploaded)
  const percentage = (displayBytesUploaded / bytesTotal * 100).toFixed(2)
  progress.value = Math.min(100, Number(percentage))
  updateRealtimeSpeed(displayBytesUploaded, now, force)
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

  averageBandwidthUtilization.value = formatDisplayUtilization(averageSpeedMbps)
  estimatedRemainingText.value = '已完成'

  uploadStats.value = {
    startedAt,
    finishedAt,
    durationText: formatDuration(durationMs),
    confirmedBytes: statsBytes,
    averageSpeedMbps: formatDisplaySpeedMbps(averageSpeedMbps),
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
  const utilizationValue = Number.isFinite(utilization) ? Math.min(100, utilization) : Number(formatUtilization(averageSpeed))
  averageBandwidthUtilization.value = utilizationValue >= SATURATED_UTILIZATION_THRESHOLD
    ? '接近满载'
    : `${utilizationValue.toFixed(1)}%`
  estimatedRemainingText.value = '已完成'

  uploadStats.value = {
    ...uploadStats.value,
    startedAt: serverStartedAt && !Number.isNaN(serverStartedAt.getTime()) ? serverStartedAt : uploadStats.value.startedAt,
    finishedAt: serverFinishedAt && !Number.isNaN(serverFinishedAt.getTime()) ? serverFinishedAt : uploadStats.value.finishedAt,
    durationText: formatDuration(durationMs),
    confirmedBytes,
    averageSpeedMbps: formatDisplaySpeedMbps(averageSpeed),
    averageBandwidthUtilization: Number.isFinite(utilizationValue) ? utilizationValue.toFixed(1) : formatUtilization(averageSpeed)
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

.file-summary {
  margin: 0 0 10px;
  color: #303133;
  font-size: 14px;
  font-weight: 500;
  overflow-wrap: anywhere;
}

.actions {
  margin-top: 15px;
}

.bandwidth-panel {
  margin: 16px auto 0;
  max-width: 820px;
}

.metric-strip {
  display: grid;
  grid-template-columns: minmax(190px, 1.3fr) repeat(3, minmax(0, 1fr));
  text-align: left;
  border: 1px solid #dcdfe6;
  border-radius: 6px;
  overflow: hidden;
  background: #fff;
}

.metric-item {
  min-width: 0;
  min-height: 74px;
  padding: 12px 14px;
  border-right: 1px solid #ebeef5;
  display: flex;
  flex-direction: column;
  justify-content: center;
}

.metric-item:last-child {
  border-right: 0;
}

.metric-primary {
  background: #f5f9ff;
}

.metric-label {
  display: block;
  margin-bottom: 6px;
  color: #909399;
  font-size: 12px;
}

.metric-item strong {
  display: flex;
  align-items: baseline;
  gap: 4px;
  color: #303133;
  font-size: 18px;
  font-weight: 600;
  word-break: break-word;
}

.metric-primary strong {
  color: #1f5fbf;
  font-size: 24px;
}

.metric-item strong span,
.metric-item strong em {
  color: #606266;
  font-size: 12px;
  font-style: normal;
  font-weight: 500;
}

.usage-meter {
  height: 5px;
  margin-top: 8px;
  border-radius: 999px;
  background: #edf2f7;
  overflow: hidden;
}

.usage-meter span {
  display: block;
  height: 100%;
  border-radius: inherit;
  background: #409eff;
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

.speed-bars {
  display: flex;
  align-items: flex-end;
  gap: 3px;
  height: 120px;
  padding: 0 2px;
  border-bottom: 1px solid #e4e7ed;
}

.speed-bars span {
  flex: 1 1 0;
  min-width: 2px;
  max-width: 8px;
  border-radius: 2px 2px 0 0;
  background: #409eff;
  transition: height 0.2s ease;
}

@media (max-width: 640px) {
  .metric-strip {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }

  .metric-item:nth-child(2n) {
    border-right: 0;
  }

  .metric-primary {
    grid-column: 1 / -1;
  }

  .metric-primary strong {
    font-size: 22px;
  }
}

.result-box {
  max-width: 820px;
  margin: 20px auto 0;
  padding: 18px;
  text-align: center;
  border: 1px solid #dcdfe6;
  border-radius: 6px;
  background: #fff;
}

.result-header {
  padding-bottom: 14px;
  border-bottom: 1px solid #ebeef5;
}

.result-status {
  color: #1f7a4d;
  font-size: 20px;
  font-weight: 700;
}

.result-subtitle {
  margin-top: 4px;
  color: #909399;
  font-size: 13px;
}

.code-display {
  position: relative;
  margin-top: 16px;
  padding: 16px;
  border: 1px solid #b7ebc6;
  border-radius: 6px;
  background: #f6ffed;
}

.code-display > span {
  display: block;
  margin-bottom: 8px;
  color: #529b2e;
  font-size: 12px;
  font-weight: 600;
}

.code {
  display: block;
  color: #1f7a4d;
  font-size: 30px;
  line-height: 1.15;
  letter-spacing: 0;
  word-break: break-all;
  font-family: monospace;
}

.copy-code-button {
  position: absolute;
  top: 12px;
  right: 12px;
}

.stats-display {
  margin-top: 16px;
}

.stats-title {
  color: #303133;
  font-weight: 600;
  margin-bottom: 10px;
  text-align: center;
}

.stats-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  border: 1px solid #ebeef5;
  border-radius: 6px;
  overflow: hidden;
}

.stats-item {
  min-width: 0;
  padding: 12px;
  border-right: 1px solid #ebeef5;
  border-bottom: 1px solid #ebeef5;
  background: #fafafa;
  text-align: center;
}

.stats-item:nth-child(3n) {
  border-right: 0;
}

.stats-item:nth-last-child(-n + 3) {
  border-bottom: 0;
}

.stats-item span {
  display: block;
  margin-bottom: 6px;
  color: #909399;
  font-size: 12px;
}

.stats-item strong {
  display: block;
  color: #303133;
  font-size: 14px;
  font-weight: 600;
  overflow-wrap: anywhere;
}

@media (max-width: 640px) {
  .result-header {
    display: block;
  }

  .result-subtitle {
    margin-top: 4px;
  }

  .code {
    font-size: 24px;
  }

  .copy-code-button {
    position: static;
    margin-top: 10px;
  }

  .stats-grid {
    grid-template-columns: 1fr;
  }

  .stats-item,
  .stats-item:nth-child(3n),
  .stats-item:nth-last-child(-n + 3) {
    border-right: 0;
    border-bottom: 1px solid #ebeef5;
  }

  .stats-item:last-child {
    border-bottom: 0;
  }
}
</style>
