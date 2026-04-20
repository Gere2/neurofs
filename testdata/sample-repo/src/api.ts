import express from 'express'
import { AuthMiddleware } from './auth'
import { UserRepository } from './user'
import { ProductController } from './products'
import { OrderController } from './orders'

const app = express()
app.use(express.json())

const userRepo = new UserRepository()
const auth = new AuthMiddleware(userRepo)

// Public routes
app.post('/auth/login', async (req, res) => {
  const { email, password } = req.body
  const user = await userRepo.findByEmail(email)
  if (!user || !(await userRepo.verifyPassword(user, password))) {
    return res.status(401).json({ error: 'Invalid credentials' })
  }
  const token = generateToken({ userId: user.id, email: user.email, role: user.role })
  res.json({ token })
})

app.post('/auth/register', async (req, res) => {
  const user = await userRepo.create(req.body)
  res.status(201).json(user)
})

// Protected routes
app.use('/api', auth.authenticate)
app.get('/api/products', ProductController.list)
app.post('/api/products', auth.requireRole('admin'), ProductController.create)
app.get('/api/orders', OrderController.list)
app.post('/api/orders', OrderController.create)

export default app
