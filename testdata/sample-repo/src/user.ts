import { db } from '../config/database'
import { hashPassword, comparePassword } from './crypto'

export interface User {
  id: string
  email: string
  role: string
  passwordHash: string
  createdAt: Date
}

export interface CreateUserInput {
  email: string
  password: string
  role?: string
}

export class UserRepository {
  async findById(id: string): Promise<User | null> {
    return db.query('SELECT * FROM users WHERE id = $1', [id])
  }

  async findByEmail(email: string): Promise<User | null> {
    return db.query('SELECT * FROM users WHERE email = $1', [email])
  }

  async create(input: CreateUserInput): Promise<User> {
    const hash = await hashPassword(input.password)
    return db.query(
      'INSERT INTO users (email, password_hash, role) VALUES ($1, $2, $3) RETURNING *',
      [input.email, hash, input.role ?? 'user']
    )
  }

  async verifyPassword(user: User, password: string): Promise<boolean> {
    return comparePassword(password, user.passwordHash)
  }
}
