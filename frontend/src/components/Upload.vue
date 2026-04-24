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
        <span>分片大小：{{ formatSize(UPLOAD_CHUNK_SIZE) }}</span>
      </div>

      <div class="actions">
        <el-button type="primary" @click="startUpload" v-if="!uploading && !uploaded">
          开始上传
        </el-button>
        <el-button type="warning" @click="pauseUpload" v-if="uploading">
          暂停
        </el-button>
        <el-button type="primary" @click="resumeUpload" v-if="paused">
          继续
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
            <p>平均速度：{{ uploadStats.averageSpeedMbps }} Mbps</p>
          </div>
        </template>
      </el-result>
    </div>
  </div>
</template>

<script setup>
import { ref } from 'vue'
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
const pausedAt = ref(null)
const pausedDurationMs = ref(0)
const currentSpeedMbps = ref('0.00')

const UPLOAD_CHUNK_SIZE = 64 * 1024 * 1024
const SPEED_WINDOW_MS = 10 * 1000

let upload = null
let speedSamples = []

const resetUploadState = () => {
  progress.value = 0
  uploading.value = false
  paused.value = false
  uploaded.value = false
  shareCode.value = ''
  uploadStats.value = null
  uploadStartedAt.value = null
  pausedAt.value = null
  pausedDurationMs.value = 0
  currentSpeedMbps.value = '0.00'
  speedSamples = []
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

const resetSpeedWindow = () => {
  currentSpeedMbps.value = '0.00'
  speedSamples = []
}

const updateRealtimeSpeed = (bytesUploaded) => {
  const now = Date.now()
  speedSamples.push({ time: now, bytes: bytesUploaded })
  speedSamples = speedSamples.filter(sample => now - sample.time <= SPEED_WINDOW_MS)

  if (speedSamples.length < 2) {
    currentSpeedMbps.value = '0.00'
    return
  }

  const first = speedSamples[0]
  const last = speedSamples[speedSamples.length - 1]
  const elapsedSeconds = (last.time - first.time) / 1000
  const uploadedBytes = last.bytes - first.bytes

  currentSpeedMbps.value = elapsedSeconds > 0 && uploadedBytes > 0
    ? ((uploadedBytes * 8) / elapsedSeconds / 1000 / 1000).toFixed(2)
    : '0.00'
}

const finishStats = () => {
  const finishedAt = new Date()
  const startedAt = uploadStartedAt.value || finishedAt
  const durationMs = Math.max(
    0,
    finishedAt.getTime() - startedAt.getTime() - pausedDurationMs.value
  )

  const averageSpeedMbps = durationMs > 0
    ? ((file.value.size * 8) / (durationMs / 1000) / 1000 / 1000).toFixed(2)
    : '0.00'

  uploadStats.value = {
    startedAt,
    finishedAt,
    durationText: formatDuration(durationMs),
    averageSpeedMbps
  }
}

const startUpload = () => {
  if (!file.value) return

  uploading.value = true
  uploaded.value = false
  uploadStats.value = null

  if (!uploadStartedAt.value) {
    uploadStartedAt.value = new Date()
    pausedDurationMs.value = 0
  }

  if (pausedAt.value) {
    pausedDurationMs.value += Date.now() - pausedAt.value.getTime()
    pausedAt.value = null
  }

  const headers = buildHeaders()

  upload = new tus.Upload(file.value, {
    endpoint: '/files/',
    chunkSize: UPLOAD_CHUNK_SIZE,
    retryDelays: [0, 3000, 5000, 10000, 20000],
    headers,
    metadata: {
      filename: file.value.name,
      filetype: file.value.type
    },
    onError: (error) => {
      console.error('Failed because:', error)
      ElMessage.error('上传失败: ' + error.message)
      uploading.value = false
    },
    onProgress: (bytesUploaded, bytesTotal) => {
      const percentage = (bytesUploaded / bytesTotal * 100).toFixed(2)
      progress.value = Number(percentage)
      updateRealtimeSpeed(bytesUploaded)
    },
    onSuccess: async () => {
      uploading.value = false
      uploaded.value = true
      finishStats()

      const uploadUrl = upload.url
      const uploadId = uploadUrl.substring(uploadUrl.lastIndexOf('/') + 1)

      try {
        const res = await axios.post('/api/get-code', { upload_id: uploadId })
        shareCode.value = res.data.code
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
    upload.start()
  })
}

const pauseUpload = () => {
  if (upload) {
    upload.abort()
    uploading.value = false
    paused.value = true
    pausedAt.value = new Date()
    resetSpeedWindow()
  }
}

const resumeUpload = () => {
  if (upload) {
    if (pausedAt.value) {
      pausedDurationMs.value += Date.now() - pausedAt.value.getTime()
      pausedAt.value = null
    }
    resetSpeedWindow()
    upload.start()
    uploading.value = true
    paused.value = false
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
  justify-content: center;
  gap: 18px;
  margin-top: 10px;
  color: #606266;
  font-size: 13px;
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
