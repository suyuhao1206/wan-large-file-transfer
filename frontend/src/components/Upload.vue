<template>
  <div class="upload-container">
    <el-upload
      class="upload-demo"
      drag
      action=""
      :auto-upload="false"
      :disabled="uploading || paused || finalizing"
      :on-change="handleFileChange"
      :show-file-list="false"
    >
      <el-icon class="el-icon--upload"><UploadFilled /></el-icon>
      <div class="el-upload__text">
        将文件拖到此处，或 <em>点击选择</em>
      </div>
    </el-upload>

    <div v-if="file" class="file-status">
      <p class="file-summary">{{ file.name }} ({{ formatSize(file.size) }})</p>
      <el-progress :percentage="progress" />

      <div class="upload-phase" v-if="uploading || paused || uploaded || finalizing">
        <span>{{ uploadStatusText }}</span>
        <strong v-if="totalChunks">{{ finishedChunks }}/{{ totalChunks }}</strong>
      </div>

      <div class="bandwidth-panel" v-if="uploading || paused || uploaded || finalizing">
        <div class="metric-strip">
          <div class="metric-item metric-primary">
            <span class="metric-label">上传速率</span>
            <strong>{{ currentSpeedText }}<em>Mbps</em></strong>
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
            <span>最近 2 分钟</span>
          </div>
          <div
            class="speed-bars"
            role="img"
            aria-label="最近 2 分钟上传速度柱状图"
            :style="{ '--speed-bar-count': SPEED_HISTORY_BAR_COUNT }"
          >
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
        <el-button type="warning" @click="pauseUpload" v-if="uploading && !finalizing">
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
const finalizing = ref(false)
const shareCode = ref('')
const uploadStats = ref(null)
const uploadStartedAt = ref(null)
const currentSpeedText = ref('0.00')
const averageBandwidthUtilization = ref('0.0%')
const bandwidthUsagePercent = ref(0)
const estimatedRemainingText = ref('-')
const speedHistory = ref([])
const uploadStatusText = ref('')
const totalChunks = ref(0)
const finishedChunks = ref(0)

const BUSINESS_CHUNK_SIZE = 5 * 1024 * 1024 * 1024
const TUS_NETWORK_CHUNK_SIZE = 64 * 1024 * 1024
const BUSINESS_UPLOAD_CONCURRENCY = 4
const MERGE_POLL_INTERVAL_MS = 3000
const MERGE_POLL_MAX_ERRORS = 20
const SPEED_WINDOW_MS = 10 * 1000
const MIN_SPEED_SAMPLE_DURATION_MS = 1000
const RESUME_SPEED_SETTLE_MS = 3000
const UI_PROGRESS_UPDATE_MS = 500
const SPEED_HISTORY_SAMPLE_MS = 1000
const SPEED_HISTORY_WINDOW_MS = 2 * 60 * 1000
const SPEED_HISTORY_BAR_COUNT = Math.ceil(SPEED_HISTORY_WINDOW_MS / SPEED_HISTORY_SAMPLE_MS)
const SPEED_HISTORY_MAX_POINTS = SPEED_HISTORY_BAR_COUNT + 1
const FIXED_BANDWIDTH_MBPS = 100

let activeUploads = new Map()
let currentChunks = []
let chunkProgress = []
let completedChunks = []
let runToken = 0
let speedSamples = []
let realtimeBytesUploaded = 0
let displayBytesUploaded = 0
let confirmedBytesUploaded = 0
let activeUploadStartedAtMs = null
let activeUploadDurationMs = 0
let lastSpeedHistorySampleAt = 0
let lastProgressUiUpdateAt = 0
let shouldRebaseSpeedWindow = false
let speedSettlingUntilMs = 0

const bandwidthUsageWidth = computed(() => {
  const safeUtilization = Number.isFinite(bandwidthUsagePercent.value)
    ? Math.max(0, Math.min(100, bandwidthUsagePercent.value))
    : 0
  return `${safeUtilization}%`
})

const speedChartBars = computed(() => {
  if (!speedHistory.value.length) return []

  const visibleHistory = speedHistory.value.slice(-SPEED_HISTORY_BAR_COUNT)
  const emptySlotCount = Math.max(0, SPEED_HISTORY_BAR_COUNT - visibleHistory.length)
  const maxSpeed = Math.max(
    FIXED_BANDWIDTH_MBPS,
    ...visibleHistory.map(sample => sample.mbps)
  )

  return [
    ...Array(emptySlotCount).fill(null),
    ...visibleHistory
  ].map((sample, index) => {
    const mbps = sample?.mbps || 0
    const ratio = maxSpeed > 0 ? Math.min(mbps / maxSpeed, 1) : 0
    return {
      key: index,
      height: ratio > 0 ? `${Math.max(4, ratio * 100).toFixed(1)}%` : '0%'
    }
  })
})

const resetUploadState = () => {
  progress.value = 0
  uploading.value = false
  paused.value = false
  uploaded.value = false
  finalizing.value = false
  shareCode.value = ''
  uploadStats.value = null
  uploadStartedAt.value = null
  uploadStatusText.value = ''
  totalChunks.value = 0
  finishedChunks.value = 0
  activeUploads = new Map()
  currentChunks = []
  chunkProgress = []
  completedChunks = []
  runToken += 1
  resetTransferMetrics()
}

const handleFileChange = (uploadFile) => {
  file.value = uploadFile.raw
  resetUploadState()
}

const formatSize = (bytes) => {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(sizes.length - 1, Math.floor(Math.log(bytes) / Math.log(k)))
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(2))} ${sizes[i]}`
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

const calculateMbps = (bytes, durationMs) => {
  const seconds = durationMs / 1000
  return seconds > 0 && bytes > 0 ? (bytes * 8) / seconds / 1000 / 1000 : 0
}

const formatDisplaySpeedMbps = (value) => {
  return formatSpeedMbps(value)
}

const getUtilizationValue = (speedMbps) => {
  const utilization = (speedMbps / FIXED_BANDWIDTH_MBPS) * 100
  return Number.isFinite(utilization) && utilization > 0 ? utilization : 0
}

const formatUtilization = (speedMbps) => {
  return getUtilizationValue(speedMbps).toFixed(1)
}

const getBandwidthUsagePercent = (speedMbps) => {
  return Math.min(100, getUtilizationValue(speedMbps))
}

const setDisplaySpeed = (speedMbps) => {
  currentSpeedText.value = formatSpeedMbps(speedMbps)
}

const formatDisplayUtilization = (speedMbps) => {
  const utilization = getUtilizationValue(speedMbps)
  if (utilization <= 0) return '0.0%'
  return `${utilization.toFixed(1)}%`
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
  averageBandwidthUtilization.value = '0.0%'
  bandwidthUsagePercent.value = 0
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
  shouldRebaseSpeedWindow = false
  speedSettlingUntilMs = 0
}

const appendSpeedHistory = (sample, force = false) => {
  if (!Number.isFinite(sample.time) || !Number.isFinite(sample.mbps)) {
    return
  }

  if (!force && lastSpeedHistorySampleAt && sample.time - lastSpeedHistorySampleAt < SPEED_HISTORY_SAMPLE_MS) {
    return
  }

  lastSpeedHistorySampleAt = sample.time
  const windowStart = sample.time - SPEED_HISTORY_WINDOW_MS
  const nextHistory = [
    ...speedHistory.value,
    sample
  ].filter(item => item.time >= windowStart)

  speedHistory.value = nextHistory.length > SPEED_HISTORY_MAX_POINTS
    ? nextHistory.slice(nextHistory.length - SPEED_HISTORY_MAX_POINTS)
    : nextHistory
}

const isSpeedSettling = (now = performance.now()) => {
  return speedSettlingUntilMs > 0 && now < speedSettlingUntilMs
}

const rebaseSpeedWindow = (bytesUploaded = realtimeBytesUploaded, now = performance.now(), forceHistory = false) => {
  realtimeBytesUploaded = bytesUploaded
  currentSpeedText.value = '0.00'
  speedSamples = [{ time: now, bytes: realtimeBytesUploaded }]
  appendSpeedHistory({ time: now, mbps: 0 }, forceHistory)
}

const resetSpeedWindow = (now = performance.now(), waitForProgressBaseline = false) => {
  rebaseSpeedWindow(realtimeBytesUploaded, now, true)
  shouldRebaseSpeedWindow = waitForProgressBaseline
}

const beginActiveUploadTiming = (settleSpeed = false) => {
  const now = performance.now()

  if (!uploadStartedAt.value) {
    uploadStartedAt.value = new Date()
  }

  if (activeUploadStartedAtMs === null) {
    activeUploadStartedAtMs = now
  }

  speedSettlingUntilMs = settleSpeed ? now + RESUME_SPEED_SETTLE_MS : 0
  resetSpeedWindow(now, true)
}

const stopActiveUploadTiming = () => {
  speedSettlingUntilMs = 0

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
  const measuredBytesUploaded = Math.max(realtimeBytesUploaded, confirmedBytesUploaded)
  const averageSpeed = calculateMbps(measuredBytesUploaded, durationMs)

  averageBandwidthUtilization.value = formatDisplayUtilization(averageSpeed)
  bandwidthUsagePercent.value = getBandwidthUsagePercent(averageSpeed)

  if (uploaded.value || progress.value >= 100) {
    estimatedRemainingText.value = '已完成'
    return
  }

  const totalBytes = file.value?.size || 0
  const remainingBytes = Math.max(0, totalBytes - measuredBytesUploaded)
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

  const settling = isSpeedSettling(now)
  if (settling || shouldRebaseSpeedWindow || bytesUploaded < realtimeBytesUploaded) {
    shouldRebaseSpeedWindow = false
    rebaseSpeedWindow(bytesUploaded, now, !settling)
    if (!settling) {
      updateAverageMetrics()
    }
    return
  }

  if (speedSettlingUntilMs > 0) {
    speedSettlingUntilMs = 0
    rebaseSpeedWindow(bytesUploaded, now, true)
    updateAverageMetrics()
    return
  }

  realtimeBytesUploaded = bytesUploaded
  speedSamples.push({ time: now, bytes: realtimeBytesUploaded })
  speedSamples = speedSamples.filter(sample => now - sample.time <= SPEED_WINDOW_MS)

  if (speedSamples.length < 2) {
    setDisplaySpeed(0)
    return
  }

  const first = speedSamples[0]
  const last = speedSamples[speedSamples.length - 1]
  const elapsedMs = last.time - first.time
  const realtimeBytes = last.bytes - first.bytes

  if (elapsedMs < MIN_SPEED_SAMPLE_DURATION_MS) {
    setDisplaySpeed(0)
    updateAverageMetrics()
    return
  }

  const speedMbps = calculateMbps(realtimeBytes, elapsedMs)
  const displaySpeedMbps = Number.isFinite(speedMbps) && speedMbps > 0 ? speedMbps : 0
  setDisplaySpeed(displaySpeedMbps)

  appendSpeedHistory({ time: now, mbps: displaySpeedMbps }, forceHistory || speedHistory.value.length <= 1)
  updateAverageMetrics()
}

const updateUploadProgress = (bytesUploaded, bytesTotal, force = false) => {
  if (!Number.isFinite(bytesUploaded) || !Number.isFinite(bytesTotal) || bytesTotal <= 0) return

  const now = performance.now()
  const settling = isSpeedSettling(now)
  if (!force && !shouldRebaseSpeedWindow && !settling && lastProgressUiUpdateAt && now - lastProgressUiUpdateAt < UI_PROGRESS_UPDATE_MS) {
    return
  }

  lastProgressUiUpdateAt = now
  displayBytesUploaded = Math.max(displayBytesUploaded, bytesUploaded)
  const percentage = (displayBytesUploaded / bytesTotal * 100).toFixed(2)
  progress.value = Math.min(100, Number(percentage))

  if (paused.value && !uploading.value && !force) {
    rebaseSpeedWindow(bytesUploaded, now)
    return
  }

  updateRealtimeSpeed(bytesUploaded, now, force)
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
  const finalFileBytes = file.value?.size || 0
  const statsBytes = finalFileBytes > 0
    ? finalFileBytes
    : Math.max(confirmedBytesUploaded, realtimeBytesUploaded)
  const averageSpeedMbps = calculateMbps(statsBytes, durationMs)

  averageBandwidthUtilization.value = formatDisplayUtilization(averageSpeedMbps)
  bandwidthUsagePercent.value = getBandwidthUsagePercent(averageSpeedMbps)
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
    : calculateMbps(confirmedBytes, durationMs)
  const utilization = Number.isFinite(Number(metric.bandwidth_utilization)) && Number(metric.bandwidth_utilization) > 0
    ? Number(metric.bandwidth_utilization)
    : Number(formatUtilization(averageSpeed))
  const utilizationValue = Number.isFinite(utilization) ? utilization : Number(formatUtilization(averageSpeed))
  averageBandwidthUtilization.value = `${utilizationValue.toFixed(1)}%`
  bandwidthUsagePercent.value = Math.min(100, utilizationValue)
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

const createFileChunks = (sourceFile) => {
  const chunks = []
  for (let start = 0; start < sourceFile.size; start += BUSINESS_CHUNK_SIZE) {
    const end = Math.min(start + BUSINESS_CHUNK_SIZE, sourceFile.size)
    chunks.push({
      index: chunks.length,
      start,
      end,
      size: end - start,
      blob: sourceFile.slice(start, end)
    })
  }
  return chunks
}

const getUploadedBytes = () => {
  return chunkProgress.reduce((total, bytes) => total + Math.max(0, bytes || 0), 0)
}

const updateChunkProgress = (chunkIndex, bytesUploaded, force = false) => {
  chunkProgress[chunkIndex] = Math.max(chunkProgress[chunkIndex] || 0, bytesUploaded)
  updateUploadProgress(getUploadedBytes(), file.value?.size || 0, force)
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

const isRunActive = (token) => {
  return token === runToken && uploading.value && !paused.value
}

const abortActiveUploads = async (terminate = false, reason = 'aborted') => {
  const tasks = Array.from(activeUploads.values())
  const abortPromises = tasks.map(task => {
    try {
      return Promise.resolve(task.upload.abort(terminate))
    } catch (err) {
      return Promise.reject(err)
    }
  })
  await Promise.allSettled(abortPromises)
  tasks.forEach(task => task.reject(new Error(reason)))
  activeUploads.clear()
}

const uploadChunk = (chunk, token) => {
  return new Promise((resolve, reject) => {
    let settled = false
    const settleResolve = (value) => {
      if (settled) return
      settled = true
      activeUploads.delete(chunk.index)
      resolve(value)
    }
    const settleReject = (error) => {
      if (settled) return
      settled = true
      activeUploads.delete(chunk.index)
      reject(error)
    }

    const chunkNumber = String(chunk.index + 1).padStart(5, '0')
    const upload = new tus.Upload(chunk.blob, {
      endpoint: '/files/',
      chunkSize: TUS_NETWORK_CHUNK_SIZE,
      retryDelays: [0, 3000, 5000, 10000, 20000],
      headers: buildHeaders(),
      metadata: {
        filename: `${file.value.name}.part-${chunkNumber}`,
        original_filename: file.value.name,
        filetype: file.value.type || '',
        filecodebox_multipart: 'true',
        multipart_upload: 'true',
        chunk_index: String(chunk.index),
        chunk_start: String(chunk.start),
        chunk_end: String(chunk.end),
        total_size: String(file.value.size)
      },
      fingerprint: () => [
        'filecodebox-business-chunk-v1',
        file.value.name,
        file.value.size,
        file.value.lastModified || 0,
        chunk.index,
        chunk.start,
        chunk.end
      ].join(':'),
      onError: (error) => {
        settleReject(error)
      },
      onProgress: (bytesUploaded) => {
        updateChunkProgress(chunk.index, bytesUploaded)
      },
      onChunkComplete: (chunkSize) => {
        updateConfirmedSpeed(chunkSize)
      },
      onSuccess: () => {
        const uploadId = uploadIdFromUrl(upload.url)
        if (!uploadId) {
          settleReject(new Error('无法获取分片上传 ID'))
          return
        }

        chunkProgress[chunk.index] = chunk.size
        completedChunks[chunk.index] = {
          upload_id: uploadId,
          index: chunk.index,
          start: chunk.start,
          end: chunk.end,
          size: chunk.size
        }
        finishedChunks.value = completedChunks.filter(Boolean).length
        uploadStatusText.value = `正在上传分片 ${Math.min(finishedChunks.value + 1, totalChunks.value)}/${totalChunks.value}`
        updateChunkProgress(chunk.index, chunk.size, true)
        settleResolve(completedChunks[chunk.index])
      }
    })

    activeUploads.set(chunk.index, {
      upload,
      reject: settleReject
    })

    upload.findPreviousUploads()
      .then((previousUploads) => {
        if (!isRunActive(token)) {
          settleReject(new Error('upload paused'))
          return
        }

        const hasPreviousUpload = previousUploads.length > 0
        if (hasPreviousUpload) {
          upload.resumeFromPreviousUpload(previousUploads[0])
        }
        upload.start()
      })
      .catch(settleReject)
  })
}

const runConcurrent = async (items, limit, worker) => {
  let cursor = 0
  const workerCount = Math.min(limit, items.length)
  const runners = Array.from({ length: workerCount }, async () => {
    while (cursor < items.length) {
      const item = items[cursor]
      cursor += 1
      await worker(item)
    }
  })

  await Promise.all(runners)
}

const sleep = (durationMs) => new Promise(resolve => setTimeout(resolve, durationMs))

const pollMergeStatus = async (taskId, token) => {
  let consecutiveErrors = 0

  while (true) {
    if (token !== runToken) {
      throw new Error('合并轮询已取消')
    }

    await sleep(MERGE_POLL_INTERVAL_MS)

    try {
      const res = await axios.get(`/api/merge-status/${encodeURIComponent(taskId)}`, {
        headers: buildHeaders()
      })
      consecutiveErrors = 0

      const status = res.data.status
      if (status === 'success') {
        return res.data
      }
      if (status === 'failed') {
        const mergeError = new Error(res.data.error || '后端合并失败')
        mergeError.isMergeFailed = true
        throw mergeError
      }

      uploadStatusText.value = '正在合并文件，请保持页面打开'
    } catch (err) {
      if (err.isMergeFailed) {
        throw err
      }

      consecutiveErrors += 1
      uploadStatusText.value = `正在等待合并状态 ${consecutiveErrors}/${MERGE_POLL_MAX_ERRORS}`
      if (consecutiveErrors >= MERGE_POLL_MAX_ERRORS) {
        throw err
      }
    }
  }
}

const finalizeUpload = async (token) => {
  const chunks = completedChunks.map((chunk, index) => {
    if (!chunk) {
      throw new Error(`分片 ${index + 1} 尚未完成`)
    }
    return chunk
  })

  uploadStatusText.value = '正在合并文件，请保持页面打开'
  finalizing.value = true
  stopActiveUploadTiming()

  const res = await axios.post('/api/finalize-multipart', {
    filename: file.value.name,
    filetype: file.value.type || 'application/octet-stream',
    total_size: file.value.size,
    chunks
  }, {
    headers: buildHeaders()
  })

  const taskId = res.data.task_id
  if (!taskId) {
    throw new Error('后端未返回合并任务 ID')
  }

  const finalRes = res.data.status === 'success'
    ? res.data
    : await pollMergeStatus(taskId, token)

  if (!finalRes.code) {
    throw new Error('合并成功但后端未返回取件码')
  }

  shareCode.value = finalRes.code
  uploaded.value = true
  finalizing.value = false
  uploading.value = false
  paused.value = false
  uploadStatusText.value = '已完成'
  updateUploadProgress(file.value.size, file.value.size, true)
  finishStats()
  applyServerUploadMetric(finalRes.upload_metric)
}

const runUploadWorkflow = async (resume = false) => {
  const token = runToken + 1
  runToken = token
  uploading.value = true
  paused.value = false
  uploaded.value = false
  finalizing.value = false
  shareCode.value = ''
  uploadStatusText.value = resume ? '继续上传' : '正在上传'
  beginActiveUploadTiming(resume)

  try {
    const pendingChunks = currentChunks.filter(chunk => !completedChunks[chunk.index])
    if (pendingChunks.length > 0) {
      uploadStatusText.value = `正在上传分片 ${finishedChunks.value + 1}/${totalChunks.value}`
      await runConcurrent(pendingChunks, BUSINESS_UPLOAD_CONCURRENCY, chunk => uploadChunk(chunk, token))
    }

    if (!isRunActive(token)) return

    updateUploadProgress(file.value.size, file.value.size, true)
    await finalizeUpload(token)
  } catch (err) {
    if (token !== runToken || paused.value) return

    console.error('Upload error:', err)
    await abortActiveUploads(false, 'upload failed')
    stopActiveUploadTiming()
    finalizing.value = false
    uploading.value = false
    uploadStatusText.value = '上传失败'
    ElMessage.error(`上传失败: ${err.response?.data?.error || err.message}`)
  }
}

const startUpload = () => {
  if (!file.value) return
  if (file.value.size <= 0) {
    ElMessage.error('不能上传空文件')
    return
  }

  progress.value = 0
  uploaded.value = false
  paused.value = false
  finalizing.value = false
  shareCode.value = ''
  uploadStats.value = null
  uploadStartedAt.value = null
  resetTransferMetrics()

  currentChunks = createFileChunks(file.value)
  chunkProgress = Array(currentChunks.length).fill(0)
  completedChunks = Array(currentChunks.length).fill(null)
  totalChunks.value = currentChunks.length
  finishedChunks.value = 0

  runUploadWorkflow(false)
}

const pauseUpload = async () => {
  if (!uploading.value || finalizing.value) return

  runToken += 1
  uploading.value = false
  paused.value = true
  uploadStatusText.value = '已暂停'
  stopActiveUploadTiming()
  await abortActiveUploads(false, 'upload paused')
  speedSettlingUntilMs = 0
  resetSpeedWindow(performance.now(), true)
}

const resumeUpload = () => {
  if (!file.value || !paused.value) return

  runUploadWorkflow(true)
}

const stopUpload = async () => {
  runToken += 1
  const uploadIds = new Set()
  activeUploads.forEach(task => {
    const uploadId = uploadIdFromUrl(task.upload.url)
    if (uploadId) uploadIds.add(uploadId)
  })
  completedChunks.forEach(chunk => {
    if (chunk?.upload_id) uploadIds.add(chunk.upload_id)
  })

  uploading.value = false
  paused.value = false
  finalizing.value = false
  stopActiveUploadTiming()

  try {
    await abortActiveUploads(true, 'upload stopped')
    await Promise.allSettled(Array.from(uploadIds).map(uploadId => terminateUploadOnServer(uploadId)))
    resetUploadState()
    ElMessage.success('已停止上传')
  } catch (err) {
    paused.value = true
    uploadStatusText.value = '停止失败'
    ElMessage.error(`停止上传失败: ${err.response?.data?.error || err.message}`)
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

.upload-phase {
  max-width: 820px;
  min-height: 34px;
  margin: 12px auto 0;
  padding: 8px 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  color: #606266;
  font-size: 13px;
  border: 1px solid #dcdfe6;
  border-radius: 6px;
  background: #fff;
}

.upload-phase strong {
  flex: 0 0 auto;
  color: #303133;
  font-weight: 600;
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
  display: grid;
  grid-template-columns: repeat(var(--speed-bar-count), minmax(0, 1fr));
  align-items: flex-end;
  gap: 2px;
  height: 120px;
  padding: 0 2px;
  border-bottom: 1px solid #e4e7ed;
  overflow: hidden;
}

.speed-bars span {
  width: 100%;
  min-width: 0;
  border-radius: 2px 2px 0 0;
  background: #409eff;
  transition: height 0.2s ease;
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

  .upload-phase {
    align-items: flex-start;
    flex-direction: column;
  }

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
