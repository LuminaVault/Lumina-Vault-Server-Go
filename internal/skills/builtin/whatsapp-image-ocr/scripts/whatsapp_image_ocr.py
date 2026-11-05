#!/usr/bin/env python3
"""
WhatsApp Image OCR Processor
Extracts text from images received via WhatsApp using Tesseract OCR.
"""

import os
import sys
import json
import time
import uuid
import traceback
import logging
from datetime import datetime
from pathlib import Path
from typing import Dict, List, Optional, Tuple

# Add parent directory to path for imports
sys.path.append(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from hermes_tools.terminal import terminal
from hermes_tools.read_file import read_file
from hermes_tools.write_file import write_file
from hermes_tools.search_files import search_files
from hermes_tools.patch import patch
from hermes_tools.execute_code import execute_code

# Configuration
VAULT_PATH = os.environ.get('OBSIDIAN_VAULT_PATH', os.path.expanduser('~/obsidian-vault/Vault'))
VAULT_NAME = os.environ.get('OBSIDIAN_VAULT_NAME', 'Vault')
IMAGE_CACHE_DIR = os.path.join(os.path.expanduser('~'), '.hermes', 'image_cache')
OCR_LANGUAGE = os.environ.get('OCR_LANGUAGE', 'eng+deu')
SAVE_IMAGES = os.environ.get('SAVE_IMAGES', 'false').lower() == 'true'

# Setup logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger('whatsapp_image_ocr')


class WhatsAppImageOCR:
    def __init__(self):
        self.vault_path = Path(VAULT_PATH)
        self.image_cache_dir = Path(IMAGE_CACHE_DIR)
        self.setup_directories()
    
    def setup_directories(self):
        """Ensure required directories exist."""
        self.image_cache_dir.mkdir(parents=True, exist_ok=True)
        (self.vault_path / 'Raw').mkdir(parents=True, exist_ok=True)
    
    def get_whatsapp_messages(self) -> List[Dict]:
        """
        Fetch new messages from the WhatsApp bridge.
        Returns list of message events.
        """
        try:
            # Poll the WhatsApp bridge for new messages
            result = terminal(
                command=f'curl -s "http://localhost:3000/messages"',
                timeout=30
            )
            if result['exit_code'] == 0:
                output = result['output'].strip()
                if output:
                    try:
                        return json.loads(output)
                    except json.JSONDecodeError:
                        logger.error(f"Failed to parse JSON from WhatsApp bridge: {output}")
        except Exception as e:
            logger.error(f"Error fetching WhatsApp messages: {e}")
        return []
    
    def process_message(self, message: Dict) -> bool:
        """
        Process a single message event.
        Returns True if image was processed successfully.
        """
        if not message.get('hasMedia') or message.get('mediaType') != 'image':
            return False
        
        media_urls = message.get('mediaUrls', [])
        if not media_urls:
            return False
        
        image_path = media_urls[0]  # Use first image
        if not Path(image_path).exists():
            logger.warning(f"Image file not found: {image_path}")
            return False
        
        try:
            # Extract text from image using OCR
            extracted_text = self.extract_text_with_ocr(image_path)
            if not extracted_text:
                logger.warning(f"No text extracted from image: {image_path}")
                return False
            
            # Save to vault with metadata
            self.save_to_vault(
                extracted_text=extracted_text,
                message=message,
                image_path=image_path
            )
            
            logger.info(f"Successfully processed image: {image_path}")
            return True
            
        except Exception as e:
            logger.error(f"Error processing image {image_path}: {e}")
            logger.debug(traceback.format_exc())
            return False
    
    def extract_text_with_ocr(self, image_path: str) -> str:
        """
        Extract text from image using Tesseract OCR.
        """
        try:
            # Run Tesseract OCR
            result = terminal(
                command=f'tesseract "{image_path}" stdout --oem 1 --psm 3 -l {OCR_LANGUAGE} 2>&1',
                timeout=60
            )
            if result['exit_code'] == 0:
                return result['output'].strip()
            else:
                logger.error(f"OCR failed for {image_path}: {result['output']}")
                return ""
        except Exception as e:
            logger.error(f"Error running OCR on {image_path}: {e}")
            return ""
    
    def save_to_vault(self, extracted_text: str, message: Dict, image_path: str):
        """
        Save extracted text to the vault with frontmatter.
        """
        # Generate filename and path
        timestamp = datetime.now().strftime('%Y-%m-%d')
        safe_timestamp = timestamp.replace(':', '-')
        message_id = message.get('messageId', str(uuid.uuid4()))
        filename = f"{safe_timestamp} - WhatsApp Image {message_id}.md"
        raw_dir = self.vault_path / 'Raw' / 'WhatsApp Images'
        raw_dir.mkdir(exist_ok=True)
        filepath = raw_dir / filename
        
        # Build content
        captured_at = datetime.fromtimestamp(message.get('timestamp', time.time())).isoformat()
        
        # Build frontmatter
        frontmatter = f"""---
classification: Document
source: {image_path}
captured_at: {captured_at}
original_content: true
---

"""
        
        # Build body
        body = extracted_text + "\n\n*Originally captured from WhatsApp on {:%Y-%m-%d %H:%M}*".format(datetime.now())
        
        # Combine
        full_content = frontmatter + body
        
        # Write to file
        write_file(
            path=str(filepath),
            content=full_content
        )
        
        # Optional: Save image file (commented out by default)
        if SAVE_IMAGES:
            self.save_image_file(image_path, filename)
        
        # Trigger knowledge base compilation
        self.trigger_compilation()
    
    def save_image_file(self, image_path: str, markdown_filename: str):
        """
        Save the original image file to the vault (commented out by default).
        """
        # This code is commented out by default. Uncomment to enable image saving.
        # images_dir = self.vault_path / 'Raw' / 'WhatsApp Images' / 'images'
        # images_dir.mkdir(exist_ok=True)
        # image_filename = Path(image_path).name
        # destination = images_dir / image_filename
        # 
        # # Copy image to vault
        # try:
        #     import shutil
        #     shutil.copy2(image_path, destination)
        #     logger.info(f"Saved image file to: {destination}")
        # except Exception as e:
        #     logger.error(f"Failed to save image file: {e}")
        pass
    
    def trigger_compilation(self):
        """
        Trigger knowledge base compilation.
        """
        try:
            # Check if compilation is needed
            result = terminal(
                command='hermes skill_view name="kb-compile" action="compile"',
                timeout=120
            )
            if result['exit_code'] == 0:
                logger.info("Triggered knowledge base compilation")
            else:
                logger.warning("Failed to trigger compilation")
        except Exception as e:
            logger.error(f"Error triggering compilation: {e}")
    
    def process_queue(self):
        """
        Main processing loop: fetch messages and process images.
        """
        messages = self.get_whatsapp_messages()
        processed_count = 0
        
        for message in messages:
            if self.process_message(message):
                processed_count += 1
        
        logger.info(f"Processed {processed_count} images from {len(messages)} messages")
        return processed_count


def main():
    """
    Main entry point.
    """
    parser = argparse.ArgumentParser(description='WhatsApp Image OCR Processor')
    parser.add_argument('--action', type=str, default='process_queue',
                        choices=['process_queue', 'ocr_image', 'test'],
                        help='Action to perform')
    parser.add_argument('--file-path', type=str, help='Path to image file (for ocr_image action)')
    parser.add_argument('--message', type=str, help='Message data as JSON (for ocr_image action)')
    
    args = parser.parse_args()
    
    ocr_processor = WhatsAppImageOCR()
    
    try:
        if args.action == 'process_queue':
            # Process all pending WhatsApp messages
            processed = ocr_processor.process_queue()
            return 0 if processed > 0 else 1
        
        elif args.action == 'ocr_image':
            # Process a single image file
            if not args.file_path or not Path(args.file_path).exists():
                logger.error(f"Image file not found: {args.file_path}")
                return 1
            
            # Extract text
            extracted_text = ocr_processor.extract_text_with_ocr(args.file_path)
            if not extracted_text:
                logger.error(f"No text extracted from image: {args.file_path}")
                return 1
            
            # Parse message data if provided
            message = {}
            if args.message:
                try:
                    message = json.loads(args.message)
                except json.JSONDecodeError:
                    logger.warning(f"Invalid message JSON: {args.message}")
            
            # Save to vault
            ocr_processor.save_to_vault(extracted_text, message, args.file_path)
            logger.info(f"Successfully processed image: {args.file_path}")
            return 0
        
        elif args.action == 'test':
            # Test OCR on a sample image
            logger.info("Running OCR test...")
            # You could add test code here
            return 0
    
    except Exception as e:
        logger.error(f"Unexpected error: {e}")
        logger.debug(traceback.format_exc())
        return 1


if __name__ == '__main__':
    sys.exit(main())