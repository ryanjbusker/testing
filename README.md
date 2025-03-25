# Live Speech-to-Speech Translation Service

A real-time speech-to-speech translation service that allows speakers to communicate with audiences in different languages.

## Features

- Real-time speech recognition
- Live translation using AWS Translate
- Text-to-speech synthesis
- WebSocket-based communication
- Support for multiple languages
- Modern, responsive UI

## Prerequisites

- Go 1.21 or later
- AWS account with Translate service access
- AWS credentials (Access Key ID and Secret Access Key)

## Environment Variables

Create a `.env` file in the root directory with the following variables:

```env
AWS_ACCESS_KEY_ID=your_access_key_id
AWS_SECRET_ACCESS_KEY=your_secret_access_key
AWS_REGION=your_aws_region
```

## Local Development

1. Clone the repository:
```bash
git clone <repository-url>
cd translation-service
```

2. Install dependencies:
```bash
go mod download
```

3. Run the server:
```bash
go run main.go
```

4. Open your browser and navigate to:
- Speaker page: http://localhost:8080/speaker
- Audience page: http://localhost:8080/audience

## Deployment

### Option 1: Heroku

1. Install the Heroku CLI
2. Login to Heroku:
```bash
heroku login
```

3. Create a new Heroku app:
```bash
heroku create your-app-name
```

4. Set environment variables:
```bash
heroku config:set AWS_ACCESS_KEY_ID=your_access_key_id
heroku config:set AWS_SECRET_ACCESS_KEY=your_secret_access_key
heroku config:set AWS_REGION=your_aws_region
```

5. Deploy to Heroku:
```bash
git push heroku main
```

### Option 2: DigitalOcean App Platform

1. Create a new app in DigitalOcean App Platform
2. Connect your GitHub repository
3. Set the following environment variables:
   - AWS_ACCESS_KEY_ID
   - AWS_SECRET_ACCESS_KEY
   - AWS_REGION
4. Deploy the app

### Option 3: AWS Elastic Beanstalk

1. Install the AWS CLI and EB CLI
2. Initialize EB:
```bash
eb init
```

3. Create an environment:
```bash
eb create production
```

4. Set environment variables in the AWS Console or using the EB CLI

## Usage

1. Open the speaker page in one browser window
2. Open the audience page in another browser window
3. On the speaker page:
   - Select your language
   - Click "Start Speaking"
   - Begin speaking
4. On the audience page:
   - Select your preferred language
   - Click "Connect to Stream"
   - Listen to the translated speech

## Security Notes

- Never commit your `.env` file or expose AWS credentials
- Use environment variables for sensitive information
- Consider using AWS IAM roles for production deployments
- Enable HTTPS for production use

## License

MIT License 