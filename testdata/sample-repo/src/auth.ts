import { Request, Response, NextFunction } from 'express'
import jwt from 'jsonwebtoken'
import { config } from '../config/app.config'
import { UserRepository } from './user'

export interface AuthPayload {
  userId: string
  email: string
  role: string
}

export interface JWTConfig {
  secret: string
  expiresIn: string
}

export function generateToken(payload: AuthPayload): string {
  return jwt.sign(payload, config.jwt.secret, {
    expiresIn: config.jwt.expiresIn,
  })
}

export function verifyToken(token: string): AuthPayload | null {
  try {
    return jwt.verify(token, config.jwt.secret) as AuthPayload
  } catch {
    return null
  }
}

export class AuthMiddleware {
  private userRepo: UserRepository

  constructor(userRepo: UserRepository) {
    this.userRepo = userRepo
  }

  authenticate = async (req: Request, res: Response, next: NextFunction) => {
    const header = req.headers.authorization
    if (!header || !header.startsWith('Bearer ')) {
      return res.status(401).json({ error: 'Missing token' })
    }

    const token = header.slice(7)
    const payload = verifyToken(token)
    if (!payload) {
      return res.status(401).json({ error: 'Invalid token' })
    }

    const user = await this.userRepo.findById(payload.userId)
    if (!user) {
      return res.status(401).json({ error: 'User not found' })
    }

    req.user = user
    next()
  }

  requireRole = (role: string) => {
    return (req: Request, res: Response, next: NextFunction) => {
      if (req.user?.role !== role) {
        return res.status(403).json({ error: 'Insufficient permissions' })
      }
      next()
    }
  }
}
